// Package fireflylive brings a throwaway Firefly III instance to the point
// where it can serve API requests, and mints a token for it.
//
// It exists because Firefly documents only one way to obtain an API token:
// clicking through Options → Profile → OAuth, where the value is shown once.
// Everything an automated caller might reach for instead is unavailable —
// `php artisan tinker` is not in the image, the OAuth password grant is never
// enabled by Firefly, and the remote user guard does not apply to the API. What
// remains is the ordinary user path, which is what this package drives.
//
// One step cannot be done over HTTP and is therefore the caller's job:
//
//	php artisan passport:client --personal --no-interaction --name=ci --provider=users
//
// A fresh instance has no personal access client, and without one the token
// request fails with "Personal access client not found".
package fireflylive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// DefaultEmail and DefaultPassword are the credentials Bootstrap creates when
// the instance has no user yet. They are deliberately fixed: the instance is
// disposable, and a stable login makes a failed run reproducible by hand.
const (
	DefaultEmail    = "ci@example.test"
	DefaultPassword = "bankingsync-integration-test-pw"
)

// csrfPattern matches Laravel's hidden CSRF field. Firefly renders it into both
// the register and the profile page.
var csrfPattern = regexp.MustCompile(`name="_token"\s+value="([^"]+)"`)

// Bootstrap readies the instance at baseURL and returns a personal access token.
//
// It is idempotent: on a fresh instance it registers the first user, and on one
// that already has a user it logs in instead. That matters because the same code
// runs against a throwaway container in CI and against a long-lived instance a
// developer keeps around locally.
func Bootstrap(ctx context.Context, baseURL string) (string, error) {
	baseURL = strings.TrimRight(baseURL, "/")

	jar, err := cookiejar.New(nil)
	if err != nil {
		return "", err
	}
	c := &session{
		base: baseURL,
		http: &http.Client{Jar: jar, Timeout: 30 * time.Second},
	}

	if err := c.waitReady(ctx); err != nil {
		return "", err
	}
	if err := c.authenticate(ctx); err != nil {
		return "", err
	}
	token, err := c.mintToken(ctx)
	if err != nil {
		return "", err
	}
	if err := c.verifyToken(ctx, token); err != nil {
		return "", err
	}
	return token, nil
}

type session struct {
	base string
	http *http.Client
}

// waitReady polls the registration page rather than the login page. With no user
// yet, Firefly answers /login with a 302 to the registration form, so waiting for
// a 200 there never succeeds on a fresh instance.
func (s *session) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(3 * time.Minute)
	var last string
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		code, _, err := s.get(ctx, "/register")
		switch {
		case err != nil:
			last = err.Error()
		case code == http.StatusOK:
			return nil
		default:
			last = fmt.Sprintf("HTTP %d", code)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("firefly at %s did not become ready: last result was %s", s.base, last)
}

// authenticate registers the first user, or logs in when one already exists.
//
// The discriminator is /login: Firefly redirects it to the registration form
// while no user exists, and serves the form once one does.
func (s *session) authenticate(ctx context.Context) error {
	code, body, err := s.get(ctx, "/login")
	if err != nil {
		return err
	}
	if code == http.StatusOK {
		return s.login(ctx, body)
	}
	return s.register(ctx)
}

func (s *session) register(ctx context.Context) error {
	_, body, err := s.get(ctx, "/register")
	if err != nil {
		return err
	}
	token, err := csrf(body, "register form")
	if err != nil {
		return err
	}

	code, resp, err := s.post(ctx, "/register", url.Values{
		"_token":                {token},
		"email":                 {DefaultEmail},
		"password":              {DefaultPassword},
		"password_confirmation": {DefaultPassword},
	})
	if err != nil {
		return err
	}
	if code != http.StatusFound {
		return fmt.Errorf("registering %s: HTTP %d, %s", DefaultEmail, code, formErrors(resp))
	}
	return nil
}

func (s *session) login(ctx context.Context, loginPage string) error {
	token, err := csrf(loginPage, "login form")
	if err != nil {
		return err
	}
	code, resp, err := s.post(ctx, "/login", url.Values{
		"_token":   {token},
		"email":    {DefaultEmail},
		"password": {DefaultPassword},
	})
	if err != nil {
		return err
	}
	if code != http.StatusFound {
		return fmt.Errorf("logging in as %s: HTTP %d, %s — the instance has a user "+
			"this package did not create; point it at a disposable one",
			DefaultEmail, code, formErrors(resp))
	}
	return nil
}

// mintToken uses Passport's own JSON route, the same one Firefly's profile page
// calls. It needs a CSRF token from an authenticated page, not from the login
// form, because Laravel rotates the token on login.
func (s *session) mintToken(ctx context.Context) (string, error) {
	code, profile, err := s.get(ctx, "/profile")
	if err != nil {
		return "", err
	}
	if code != http.StatusOK {
		return "", fmt.Errorf("opening /profile: HTTP %d — the session did not survive login", code)
	}
	token, err := csrf(profile, "profile page")
	if err != nil {
		return "", err
	}

	req, err := s.request(ctx, http.MethodPost, "/oauth/personal-access-tokens",
		strings.NewReader(`{"name":"bankingsync-integration","scopes":[]}`))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-TOKEN", token)

	status, body, err := s.do(req)
	if err != nil {
		return "", err
	}

	var out struct {
		AccessToken string `json:"accessToken"`
		Message     string `json:"message"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if out.AccessToken == "" {
		if strings.Contains(out.Message, "Personal access client not found") {
			return "", fmt.Errorf("%w: run "+
				"`php artisan passport:client --personal --no-interaction --name=ci --provider=users` "+
				"in the container first", ErrNoPersonalAccessClient)
		}
		return "", fmt.Errorf("minting a token: HTTP %d, %s", status, excerpt(body))
	}
	return out.AccessToken, nil
}

// ErrNoPersonalAccessClient reports the one setup step that cannot be done over
// HTTP, so a caller can tell it apart from a genuine failure.
var ErrNoPersonalAccessClient = errors.New("firefly has no personal access client")

// verifyToken is the difference between "a token was issued" and "a token that
// works against the API". Without it a broken bootstrap would surface much later
// as an unexplained 401 inside a test.
func (s *session) verifyToken(ctx context.Context, token string) error {
	req, err := s.request(ctx, http.MethodGet, "/api/v1/about", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	status, body, err := s.do(req)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("token was issued but /api/v1/about answered HTTP %d: %s", status, excerpt(body))
	}
	if !strings.Contains(body, `"version"`) {
		return fmt.Errorf("/api/v1/about did not report a version: %s", excerpt(body))
	}
	return nil
}

func (s *session) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, body)
	if err != nil {
		return nil, err
	}
	// Without this header Laravel renders failures as a 302 to the app root
	// instead of a JSON error, which turns every diagnosable problem into an
	// unexplained redirect.
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (s *session) get(ctx context.Context, path string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+path, nil)
	if err != nil {
		return 0, "", err
	}
	return s.do(req)
}

func (s *session) post(ctx context.Context, path string, form url.Values) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.base+path,
		strings.NewReader(form.Encode()))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return s.do(req)
}

// do never follows redirects. A 302 is meaningful here — it is how a successful
// registration and a successful login announce themselves — and following it
// would replace that signal with the HTML of whatever page came next.
func (s *session) do(req *http.Request) (int, string, error) {
	client := *s.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, "", fmt.Errorf("%s %s: reading body: %w", req.Method, req.URL.Path, err)
	}
	return resp.StatusCode, string(body), nil
}

func csrf(page, where string) (string, error) {
	m := csrfPattern.FindStringSubmatch(page)
	if m == nil {
		return "", fmt.Errorf("no CSRF token in the %s; Firefly may have changed the form: %s",
			where, excerpt(page))
	}
	return m[1], nil
}

// formErrors pulls Laravel's validation messages out of a rejected form, so a
// failure names its own cause instead of dumping a page of HTML.
func formErrors(page string) string {
	var out []string
	for _, m := range regexp.MustCompile(`<li>([^<]{3,200})</li>`).FindAllStringSubmatch(page, 8) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	if len(out) == 0 {
		return excerpt(page)
	}
	return strings.Join(out, "; ")
}

func excerpt(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
