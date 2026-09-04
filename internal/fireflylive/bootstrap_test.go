package fireflylive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFirefly answers the four routes Bootstrap depends on, with the two pieces
// of state that decide its branching: whether a user exists and whether the
// personal access client has been created.
//
// It is not a Firefly model and makes no claim to be one — the live job is what
// proves the real instance behaves this way. What it is for is the control flow
// around those answers, which is the part that broke repeatedly by hand and which
// the live job can only exercise one branch of per run.
type fakeFirefly struct {
	mu sync.Mutex

	hasUser           bool
	hasPersonalClient bool
	loggedIn          bool

	readyAfter  int // number of requests to answer 503 before coming up
	seen        int
	aboutStatus int
	omitCSRF    bool
}

const (
	registerCSRF = "csrf-from-register-form"
	profileCSRF  = "csrf-from-profile-page"
	issuedToken  = "eyJ0eXAiOiJKV1QiLCJhbGciOiJSUzI1NiJ9.fake"
)

func (f *fakeFirefly) form(token string) string {
	if f.omitCSRF {
		return `<!DOCTYPE html><html><body><form method="post"></form></body></html>`
	}
	return fmt.Sprintf(`<!DOCTYPE html><html><body><form method="post">`+
		`<input type="hidden" name="_token" value="%s"></form></body></html>`, token)
}

func (f *fakeFirefly) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.seen++
	if f.seen <= f.readyAfter {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	switch {
	case r.URL.Path == "/register" && r.Method == http.MethodGet:
		fmt.Fprint(w, f.form(registerCSRF))

	case r.URL.Path == "/register" && r.Method == http.MethodPost:
		if err := r.ParseForm(); err != nil || r.PostForm.Get("_token") != registerCSRF {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `<ul><li>The token is invalid.</li></ul>`)
			return
		}
		f.hasUser, f.loggedIn = true, true
		w.Header().Set("Location", "/")
		w.WriteHeader(http.StatusFound)

	case r.URL.Path == "/login" && r.Method == http.MethodGet:
		// Firefly sends the visitor to the registration form while no user
		// exists. This 302 is what waitReady must not be pointed at, and what
		// authenticate uses to tell the two cases apart.
		if !f.hasUser {
			w.Header().Set("Location", "/register")
			w.WriteHeader(http.StatusFound)
			return
		}
		fmt.Fprint(w, f.form(registerCSRF))

	case r.URL.Path == "/login" && r.Method == http.MethodPost:
		_ = r.ParseForm()
		if r.PostForm.Get("email") != DefaultEmail || r.PostForm.Get("password") != DefaultPassword {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, `<ul><li>These credentials do not match our records.</li></ul>`)
			return
		}
		f.loggedIn = true
		w.Header().Set("Location", "/")
		w.WriteHeader(http.StatusFound)

	case r.URL.Path == "/profile":
		if !f.loggedIn {
			w.Header().Set("Location", "/login")
			w.WriteHeader(http.StatusFound)
			return
		}
		fmt.Fprint(w, f.form(profileCSRF))

	case r.URL.Path == "/oauth/personal-access-tokens" && r.Method == http.MethodPost:
		if r.Header.Get("X-CSRF-TOKEN") != profileCSRF {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		if !f.hasPersonalClient {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"message":"Personal access client not found for 'users' user provider."}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"accessToken":%q,"token":{"id":"1"}}`, issuedToken)

	case r.URL.Path == "/api/v1/about":
		if r.Header.Get("Authorization") != "Bearer "+issuedToken {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"message":"Unauthenticated."}`)
			return
		}
		if f.aboutStatus != 0 {
			w.WriteHeader(f.aboutStatus)
			fmt.Fprint(w, `{"message":"nope"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":{"version":"6.6.6","api_version":"2.1.0","os":"Linux","driver":"sqlite"}}`)

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func serve(t *testing.T, f *fakeFirefly) string {
	t.Helper()
	ts := httptest.NewServer(f)
	t.Cleanup(ts.Close)
	return ts.URL
}

func TestBootstrap_registersTheFirstUserOnAFreshInstance(t *testing.T) {
	f := &fakeFirefly{hasPersonalClient: true}
	got, err := Bootstrap(context.Background(), serve(t, f))
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if got != issuedToken {
		t.Errorf("token = %q, want %q", got, issuedToken)
	}
	if !f.hasUser {
		t.Error("no user was created")
	}
}

// TestBootstrap_logsInWhenAUserAlreadyExists is the idempotence the package
// promises: the same code runs against a throwaway container in CI and against
// an instance a developer keeps between runs.
func TestBootstrap_logsInWhenAUserAlreadyExists(t *testing.T) {
	f := &fakeFirefly{hasPersonalClient: true, hasUser: true}
	if _, err := Bootstrap(context.Background(), serve(t, f)); err != nil {
		t.Fatalf("Bootstrap against an instance that already has a user: %v", err)
	}
	if !f.loggedIn {
		t.Error("the login branch was not taken")
	}
}

// TestBootstrap_toleratesATrailingSlash guards the one bit of input normalising.
// A base URL pasted from a browser carries the slash, and without the trim every
// path would be requested as //register.
func TestBootstrap_toleratesATrailingSlash(t *testing.T) {
	f := &fakeFirefly{hasPersonalClient: true}
	if _, err := Bootstrap(context.Background(), serve(t, f)+"/"); err != nil {
		t.Fatalf("Bootstrap with a trailing slash: %v", err)
	}
}

// TestBootstrap_namesTheMissingPersonalAccessClient covers the one setup step
// that has no HTTP equivalent. It has to be distinguishable from a real failure,
// because the fix is a command in the container rather than a code change.
func TestBootstrap_namesTheMissingPersonalAccessClient(t *testing.T) {
	f := &fakeFirefly{hasPersonalClient: false}
	_, err := Bootstrap(context.Background(), serve(t, f))
	if !errors.Is(err, ErrNoPersonalAccessClient) {
		t.Fatalf("want ErrNoPersonalAccessClient, got %v", err)
	}
	if !strings.Contains(err.Error(), "passport:client --personal") {
		t.Errorf("the error does not carry the command that fixes it: %v", err)
	}
}

// TestBootstrap_failsWhenTheIssuedTokenDoesNotWork is why verifyToken exists.
// Without it a broken bootstrap surfaces much later as an unexplained 401 in the
// middle of a test, with nothing pointing back here.
func TestBootstrap_failsWhenTheIssuedTokenDoesNotWork(t *testing.T) {
	f := &fakeFirefly{hasPersonalClient: true, aboutStatus: http.StatusForbidden}
	_, err := Bootstrap(context.Background(), serve(t, f))
	if err == nil {
		t.Fatal("a token that the API rejects was reported as success")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("the error does not carry the status: %v", err)
	}
}

// TestBootstrap_reportsAChangedForm is the failure mode a Firefly upgrade would
// produce. The requirement is only that it says so instead of failing somewhere
// downstream with an empty token.
func TestBootstrap_reportsAChangedForm(t *testing.T) {
	f := &fakeFirefly{hasPersonalClient: true, omitCSRF: true}
	_, err := Bootstrap(context.Background(), serve(t, f))
	if err == nil {
		t.Fatal("a form without a CSRF field was accepted")
	}
	if !strings.Contains(err.Error(), "CSRF") {
		t.Errorf("the error does not name the cause: %v", err)
	}
}

// TestBootstrap_waitsForTheMigrations exercises the retry rather than the happy
// path: the container answers long before Firefly has migrated, so a bootstrap
// that gave up on the first non-200 would fail on every cold start.
func TestBootstrap_waitsForTheMigrations(t *testing.T) {
	f := &fakeFirefly{hasPersonalClient: true, readyAfter: 1}
	if _, err := Bootstrap(context.Background(), serve(t, f)); err != nil {
		t.Fatalf("Bootstrap did not wait for the instance to come up: %v", err)
	}
}

// TestBootstrap_honoursACancelledContext keeps the readiness loop from outliving
// the job that started it. Phase 0 produced exactly this: a probe that sat in a
// silent loop for three minutes with nothing to show for it.
func TestBootstrap_honoursACancelledContext(t *testing.T) {
	f := &fakeFirefly{readyAfter: 1 << 30}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := Bootstrap(ctx, serve(t, f)); err == nil {
		t.Fatal("expected a cancellation error")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the readiness loop ignored the deadline, waited %s", elapsed)
	}
}

func TestFormErrors_liftsLaravelsValidationMessages(t *testing.T) {
	page := `<html><body><ul class="list-unstyled">` +
		`<li>The email has already been taken.</li>` +
		`<li>The password must be at least 16 characters.</li></ul></body></html>`

	got := formErrors(page)
	for _, want := range []string{"email has already been taken", "at least 16 characters"} {
		if !strings.Contains(got, want) {
			t.Errorf("formErrors() = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "<li>") {
		t.Errorf("formErrors() returned markup: %q", got)
	}
}

// TestFormErrors_fallsBackToTheBody keeps a failure diagnosable when the page is
// not a validation error at all — a stack trace, or a proxy's error page.
func TestFormErrors_fallsBackToTheBody(t *testing.T) {
	if got := formErrors("<html><body>502 Bad Gateway</body></html>"); !strings.Contains(got, "502") {
		t.Errorf("formErrors() = %q, want the body", got)
	}
}

func TestExcerpt_collapsesAndTruncates(t *testing.T) {
	got := excerpt("  a\n\tb   c  ")
	if got != "a b c" {
		t.Errorf("excerpt() = %q, want %q", got, "a b c")
	}
	long := excerpt(strings.Repeat("x", 500))
	if len([]rune(long)) != 301 {
		t.Errorf("excerpt() length = %d runes, want 300 plus the ellipsis", len([]rune(long)))
	}
}
