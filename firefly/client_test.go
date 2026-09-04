package firefly

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return New(ts.URL, "tok-1", WithHTTPClient(ts.Client()), WithBackoffBase(time.Millisecond))
}

func TestClient_sendsBearerOnEveryRequest(t *testing.T) {
	var seen int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		atomic.AddInt32(&seen, 1)
		_, _ = w.Write([]byte(`{"data":{"version":"6.6.6"}}`))
	})

	v, err := c.About(context.Background())
	if err != nil {
		t.Fatalf("About: %v", err)
	}
	if v != "6.6.6" {
		t.Errorf("version: got %q", v)
	}
	if atomic.LoadInt32(&seen) != 1 {
		t.Error("the request was not authenticated")
	}
}

func TestClient_pagesUntilTotalPages(t *testing.T) {
	const perPage, totalPages = 2, 3
	var requested []string

	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		requested = append(requested, r.URL.Query().Get("page"))
		items := []json.RawMessage{}
		for i := 0; i < perPage; i++ {
			items = append(items, json.RawMessage(fmt.Sprintf(`{"id":"%d"}`, (page-1)*perPage+i)))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": items,
			"meta": map[string]any{"pagination": map[string]any{
				"total": perPage * totalPages, "count": perPage, "per_page": perPage,
				"current_page": page, "total_pages": totalPages,
			}},
		})
	})

	got, err := c.GetPaged(context.Background(), "/api/v1/accounts", url.Values{"type": {"asset"}})
	if err != nil {
		t.Fatalf("GetPaged: %v", err)
	}
	if len(got) != perPage*totalPages {
		t.Fatalf("got %d items, want %d — pagination must follow meta, not the requested limit", len(got), perPage*totalPages)
	}
	if len(requested) != totalPages {
		t.Errorf("requested pages %v, want %d requests", requested, totalPages)
	}
}

func TestClient_retriesRateLimitAndHonoursRetryAfter(t *testing.T) {
	var calls int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"version":"6.6.6"}}`))
	})

	if _, err := c.About(context.Background()); err != nil {
		t.Fatalf("About: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("calls: got %d, want 2 (one rate-limited, one retried)", got)
	}
}

func TestClient_postIsNotRetriedOnServerError(t *testing.T) {
	var calls int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	})

	_, err := c.Post(context.Background(), "/api/v1/transactions", map[string]any{"x": 1})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("a POST must never be retried on 5xx: the transaction may already exist. calls: %d", got)
	}
}

func TestClient_putIsRetriedOnServerError(t *testing.T) {
	var calls int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"data":{}}`))
	})

	if _, err := c.Put(context.Background(), "/api/v1/transactions/1", map[string]any{"x": 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("an idempotent PUT should be retried, calls: %d", got)
	}
}

func TestClient_postIsRetriedOnRateLimit(t *testing.T) {
	var calls int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"id":"1"}}`))
	})

	if _, err := c.Post(context.Background(), "/api/v1/transactions", map[string]any{}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("a rate-limited POST was declined, not processed, so it is safe to retry. calls: %d", got)
	}
}

func TestClient_errorCarriesStatusAndBody(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"Duplicate of transaction #42."}`))
	})

	_, err := c.Post(context.Background(), "/api/v1/transactions", map[string]any{})
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.Status != http.StatusUnprocessableEntity {
		t.Errorf("status: got %d", apiErr.Status)
	}
	if apiErr.Body == "" {
		t.Error("the body must survive: telling a duplicate rejection from a real failure depends on it")
	}
}

func TestClient_paginationIsBounded(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []json.RawMessage{json.RawMessage(`{"id":"1"}`)},
			"meta": map[string]any{"pagination": map[string]any{
				"current_page": page, "total_pages": maxPages + 10,
			}},
		})
	})

	if _, err := c.GetPaged(context.Background(), "/api/v1/transactions", nil); err == nil {
		t.Fatal("an endless pagination must abort rather than loop forever")
	}
}

func TestClient_respectsContextCancellation(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := c.About(ctx); err == nil {
		t.Fatal("expected a cancellation error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("backoff ignored the deadline, waited %s", elapsed)
	}
}

func TestUnwrap(t *testing.T) {
	got, err := Unwrap([]byte(`{"data":{"id":"7"}}`))
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if string(got) != `{"id":"7"}` {
		t.Errorf("got %s", got)
	}
}

// TestClient_refusesToFollowARedirect pins the redirect policy from New.
//
// Firefly's own error rendering is the reason: without Accept: application/json
// Laravel answers with a 302 to the app root rather than a JSON error. The client
// sends that header, so the case should not arise — but a redirect that is
// followed silently ends with an HTML login page being decoded as JSON, and the
// resulting error names neither the redirect nor the status that caused it.
func TestClient_refusesToFollowARedirect(t *testing.T) {
	var landed int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			atomic.AddInt32(&landed, 1)
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<!DOCTYPE html><html><body>Sign in</body></html>")
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	})

	_, err := c.Get(context.Background(), "/api/v1/about", nil)
	if err == nil {
		t.Fatal("a 302 was reported as success")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want an *APIError carrying the redirect, got %T: %v", err, err)
	}
	if apiErr.Status != http.StatusFound {
		t.Errorf("status = %d, want 302", apiErr.Status)
	}
	if n := atomic.LoadInt32(&landed); n != 0 {
		t.Errorf("the client followed the redirect %d time(s); the HTML would reach the JSON decoder", n)
	}
}

// TestClient_keepsAnExplicitRedirectPolicy guards the nil check. A caller that
// supplies its own CheckRedirect through WithHTTPClient must keep it, otherwise
// New would be rewriting the policy of a client it does not own.
func TestClient_keepsAnExplicitRedirectPolicy(t *testing.T) {
	sentinel := fmt.Errorf("caller policy")
	h := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return sentinel }}
	c := New("http://example.invalid", "tok", WithHTTPClient(h))

	if c.http.CheckRedirect == nil {
		t.Fatal("the caller's policy was dropped")
	}
	if got := c.http.CheckRedirect(nil, nil); got != sentinel {
		t.Errorf("New replaced the caller's redirect policy: got %v", got)
	}
}
