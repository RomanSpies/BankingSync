package fireflylive

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func setEnv(t *testing.T, url, runID, consent string) {
	t.Helper()
	t.Setenv("FIREFLY_LIVE_URL", url)
	t.Setenv("FIREFLY_LIVE_RUN_ID", runID)
	t.Setenv("FIREFLY_LIVE_DESTRUCTIVE", consent)
}

func TestEnvFromOS_acceptsACompleteEnvironment(t *testing.T) {
	setEnv(t, "http://127.0.0.1:18080/", "42-99", "yes")

	e, err := EnvFromOS()
	if err != nil {
		t.Fatalf("EnvFromOS: %v", err)
	}
	if e.BaseURL != "http://127.0.0.1:18080" {
		t.Errorf("BaseURL = %q, want the trailing slash removed", e.BaseURL)
	}
	if e.RunID != "42-99" {
		t.Errorf("RunID = %q", e.RunID)
	}
}

// TestEnvFromOS_namesTheMissingPiece matters more than it looks. Whoever hits
// this is running a tagged build by hand, and the difference between "no live
// Firefly instance configured" and a message naming the variable is a round trip.
func TestEnvFromOS_namesTheMissingPiece(t *testing.T) {
	for name, tc := range map[string]struct{ url, runID, consent, want string }{
		"no url":       {"", "1", "yes", "FIREFLY_LIVE_URL"},
		"no run id":    {"http://x", "", "yes", "FIREFLY_LIVE_RUN_ID"},
		"no consent":   {"http://x", "1", "", "FIREFLY_LIVE_DESTRUCTIVE"},
		"consent typo": {"http://x", "1", "true", "FIREFLY_LIVE_DESTRUCTIVE"},
	} {
		t.Run(name, func(t *testing.T) {
			setEnv(t, tc.url, tc.runID, tc.consent)

			_, err := EnvFromOS()
			if !errors.Is(err, ErrNoInstance) {
				t.Fatalf("want ErrNoInstance, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not name %s: %v", tc.want, err)
			}
		})
	}
}

// TestEnvFromOS_acceptsConsentInAnyCase keeps a shell that upper-cases its
// variables from failing a run for the wrong reason.
func TestEnvFromOS_acceptsConsentInAnyCase(t *testing.T) {
	setEnv(t, "http://x", "1", "YES")
	if _, err := EnvFromOS(); err != nil {
		t.Errorf("EnvFromOS with consent spelled YES: %v", err)
	}
}

func TestNamespace_isStableAndSeparatesRunsAndTests(t *testing.T) {
	a := Namespace("run-1", "TestSomething")
	if a != Namespace("run-1", "TestSomething") {
		t.Error("Namespace is not stable across calls")
	}
	if !strings.HasPrefix(a, NamePrefix) {
		t.Errorf("Namespace() = %q, want the %q prefix that AssertDisposable looks for", a, NamePrefix)
	}
	if !strings.HasSuffix(a, "-") {
		t.Errorf("Namespace() = %q, want a trailing separator so account names stay readable", a)
	}
	if a == Namespace("run-2", "TestSomething") {
		t.Error("two runs share a namespace; they would steal each other's accounts")
	}
	if a == Namespace("run-1", "TestSomethingElse") {
		t.Error("two tests share a namespace; they would steal each other's accounts")
	}
}

// accountsServer answers the account listing in pages, so the disposability
// check can be tested for the case it actually guards against.
func accountsServer(t *testing.T, pages [][]string) (string, *int) {
	t.Helper()
	var deleted int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// A missing or unparseable page means the first one, which is how a client
		// that does not paginate behaves. Parsing with strconv rather than Sscanf
		// makes that fallback explicit instead of an ignored error.
		page := 1
		if n, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil {
			page = n
		}
		var names []string
		if page >= 1 && page <= len(pages) {
			names = pages[page-1]
		}
		var items []string
		for _, n := range names {
			items = append(items, fmt.Sprintf(`{"attributes":{"name":%q,"type":"asset"}}`, n))
		}
		fmt.Fprintf(w, `{"data":[%s],"meta":{"pagination":{"total_pages":%d}}}`,
			strings.Join(items, ","), len(pages))
	}))
	t.Cleanup(ts.Close)
	return ts.URL, &deleted
}

func TestAssertDisposable_acceptsAnInstanceHoldingOnlyItsOwnAccounts(t *testing.T) {
	url, _ := accountsServer(t, [][]string{{NamePrefix + "aaa-Checking", NamePrefix + "bbb-Savings"}})
	if err := AssertDisposable(context.Background(), url, "tok"); err != nil {
		t.Errorf("AssertDisposable: %v", err)
	}
}

func TestAssertDisposable_refusesAForeignAccount(t *testing.T) {
	url, _ := accountsServer(t, [][]string{{NamePrefix + "aaa-Checking", "Privatkonto"}})
	err := AssertDisposable(context.Background(), url, "tok")
	if err == nil {
		t.Fatal("an instance holding a real account was accepted")
	}
	if !strings.Contains(err.Error(), "Privatkonto") {
		t.Errorf("the error does not name the account it found: %v", err)
	}
}

// TestAssertDisposable_looksPastTheFirstPage is the reason the check paginates.
// A single-page check would wave through an instance whose real accounts happen
// to sort after the ones this suite made.
func TestAssertDisposable_looksPastTheFirstPage(t *testing.T) {
	url, _ := accountsServer(t, [][]string{
		{NamePrefix + "aaa-Checking"},
		{NamePrefix + "bbb-Savings"},
		{"Gemeinschaftskonto"},
	})
	err := AssertDisposable(context.Background(), url, "tok")
	if err == nil {
		t.Fatal("a foreign account on page 3 was not noticed")
	}
	if !strings.Contains(err.Error(), "Gemeinschaftskonto") {
		t.Errorf("wrong account named: %v", err)
	}
}

// TestAssertDisposable_failsClosed covers the case where the listing itself does
// not work. Treating an unreadable instance as empty would defeat the check.
func TestAssertDisposable_failsClosed(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	if err := AssertDisposable(context.Background(), ts.URL, "tok"); err == nil {
		t.Fatal("an instance that could not be listed was treated as disposable")
	}
}

func TestDeleteTransactionGroup_sendsDELETEAndAcceptsNoContent(t *testing.T) {
	url, deleted := accountsServer(t, [][]string{{}})
	if err := DeleteTransactionGroup(context.Background(), url, "tok", "7"); err != nil {
		t.Fatalf("DeleteTransactionGroup: %v", err)
	}
	if *deleted != 1 {
		t.Errorf("DELETE requests: got %d, want 1", *deleted)
	}
}

func TestDeleteTransactionGroup_reportsAFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	err := DeleteTransactionGroup(context.Background(), ts.URL, "tok", "7")
	if err == nil {
		t.Fatal("a failed delete was reported as success")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("the error does not carry the status: %v", err)
	}
}
