package firefly

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	maxPages        = 100
	maxRateRetry    = 3
	rateRetryBase   = 2 * time.Second
	maxRetryAfter   = 60 * time.Second
	requestTimeout  = 60 * time.Second
	defaultPageSize = 100
)

// Client is a thin transport over the Firefly III v1 API. It knows about
// authentication, pagination and rate limiting, and nothing about budgets.
type Client struct {
	baseURL     string
	token       string
	http        *http.Client
	backoffBase time.Duration
	obs         obs
}

type Option func(*Client)

// WithHTTPClient replaces the underlying client, which tests use to inject a
// transport and production uses to relax TLS verification.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) { c.http = h }
}

// WithBackoffBase shortens the retry backoff. Tests use it to exercise the retry
// paths without waiting for the production delays.
func WithBackoffBase(d time.Duration) Option {
	return func(c *Client) { c.backoffBase = d }
}

func New(baseURL, token string, opts ...Option) *Client {
	c := &Client{
		baseURL:     strings.TrimRight(baseURL, "/"),
		token:       token,
		http:        &http.Client{Timeout: requestTimeout},
		backoffBase: rateRetryBase,
		obs:         newObs(),
	}
	for _, o := range opts {
		o(c)
	}

	// A redirect is never a legitimate answer from the Firefly API, so following
	// one can only do harm. Laravel renders error responses as a 302 to the app
	// root when it does not see Accept: application/json. We do send that header,
	// so this should not arise — but if a 302 ever arrived for another reason, a
	// stale session or a misconfigured APP_URL, the default client would follow it
	// without a word and hand an HTML page to the JSON decoder. Stopping at the
	// redirect turns it into an APIError carrying status 302 and the body.
	//
	// The copy is deliberate: WithHTTPClient may be handed a client the caller
	// uses elsewhere too, and quietly rewriting its redirect policy would be a
	// surprise. A nil check keeps an explicit policy from a caller intact.
	effective := *c.http
	if effective.CheckRedirect == nil {
		effective.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	c.http = &effective

	return c
}

// APIError carries a failing response so callers can tell a duplicate rejection
// apart from a genuine failure.
type APIError struct {
	Status int
	Method string
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, truncate(e.Body, 300))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

type pagination struct {
	Total       int `json:"total"`
	Count       int `json:"count"`
	PerPage     int `json:"per_page"`
	CurrentPage int `json:"current_page"`
	TotalPages  int `json:"total_pages"`
}

type listEnvelope struct {
	Data []json.RawMessage `json:"data"`
	Meta struct {
		Pagination pagination `json:"pagination"`
	} `json:"meta"`
}

type singleEnvelope struct {
	Data json.RawMessage `json:"data"`
}

// About reports the running Firefly version, doubling as the liveness probe.
func (c *Client) About(ctx context.Context) (string, error) {
	body, err := c.Get(ctx, "/api/v1/about", nil)
	if err != nil {
		return "", err
	}
	var env struct {
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return "", fmt.Errorf("decode about: %w", err)
	}
	return env.Data.Version, nil
}

func (c *Client) Get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	return c.do(ctx, http.MethodGet, path, query, nil, true)
}

// GetPaged walks every page of a list endpoint and returns the concatenated
// data entries. It trusts meta.pagination rather than the requested page size,
// because PAGINATION_PER_PAGE is a deployment setting.
func (c *Client) GetPaged(ctx context.Context, path string, query url.Values) ([]json.RawMessage, error) {
	if query == nil {
		query = url.Values{}
	} else {
		query = cloneValues(query)
	}
	if query.Get("limit") == "" {
		query.Set("limit", strconv.Itoa(defaultPageSize))
	}

	var out []json.RawMessage
	for page := 1; ; page++ {
		if page > maxPages {
			return nil, fmt.Errorf("pagination exceeded %d pages for %s — aborting to avoid an unbounded loop", maxPages, path)
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("paging cancelled after %d pages: %w", page-1, err)
		}

		query.Set("page", strconv.Itoa(page))
		body, err := c.Get(ctx, path, query)
		if err != nil {
			return nil, err
		}

		var env listEnvelope
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, fmt.Errorf("decode %s page %d: %w", path, page, err)
		}
		out = append(out, env.Data...)

		p := env.Meta.Pagination
		if p.TotalPages > 0 {
			if page >= p.TotalPages {
				return out, nil
			}
			continue
		}
		if len(env.Data) == 0 {
			return out, nil
		}
	}
}

func (c *Client) Post(ctx context.Context, path string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	return c.do(ctx, http.MethodPost, path, nil, raw, false)
}

func (c *Client) Put(ctx context.Context, path string, payload any) ([]byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	return c.do(ctx, http.MethodPut, path, nil, raw, true)
}

// Unwrap returns the "data" member of a single-object response.
func Unwrap(body []byte) (json.RawMessage, error) {
	var env singleEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return env.Data, nil
}

// do performs one request with the retry policy the method allows.
//
// GET and PUT are idempotent, so a 5xx or a lost connection may be retried. POST
// is not: a timeout may mean the transaction was created and the response was
// lost, and retrying would duplicate it. Only 429 is retried for POST, because a
// rate-limited request is one the server declined to process.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body []byte, idempotent bool) ([]byte, error) {
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	// One span per logical request rather than per attempt: a retried request is
	// still one thing the caller asked for, and the attempt count is the
	// interesting part rather than a second span that looks like extra work.
	//
	// This is the only place every Firefly request passes through, which is why
	// the instrumentation sits here instead of in an HTTP middleware. It is also
	// the only place that knows whether a retry is permitted at all.
	route := routeTemplate(path)
	ctx, span := tracer().Start(ctx, "firefly "+method+" "+route,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("http.request.method", method),
			attribute.String("url.path", route),
			attribute.Bool("firefly.idempotent", idempotent),
		))
	defer span.End()

	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != nil {
			reader = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.http.Do(req)
		if err != nil {
			if idempotent && attempt < maxRateRetry {
				span.AddEvent("retry", trace.WithAttributes(
					attribute.Int("attempt", attempt+1),
					attribute.String("reason", "transport"),
				))
				if waitErr := sleep(ctx, c.backoff(attempt)); waitErr != nil {
					return nil, waitErr
				}
				continue
			}
			err = fmt.Errorf("%s %s: %w", method, path, err)
			span.RecordError(err)
			span.SetStatus(codes.Error, "transport failed")
			return nil, err
		}

		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		c.obs.recordRequest(ctx, method, route, resp.StatusCode)

		if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRateRetry {
			wait := c.retryAfterDelay(resp.Header.Get("Retry-After"), attempt)
			log.Printf("Firefly rate limited (attempt %d/%d), waiting %s", attempt+1, maxRateRetry, wait)
			c.obs.recordRateLimited(ctx, route)
			span.AddEvent("rate_limited", trace.WithAttributes(
				attribute.Int("attempt", attempt+1),
				attribute.Float64("wait_seconds", wait.Seconds()),
			))
			if waitErr := sleep(ctx, wait); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		if resp.StatusCode >= 500 && idempotent && attempt < maxRateRetry {
			span.AddEvent("retry", trace.WithAttributes(
				attribute.Int("attempt", attempt+1),
				attribute.String("reason", "server_error"),
			))
			if waitErr := sleep(ctx, c.backoff(attempt)); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		span.SetAttributes(
			attribute.Int("http.response.status_code", resp.StatusCode),
			attribute.Int("http.request.resend_count", attempt),
		)

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			apiErr := &APIError{Status: resp.StatusCode, Method: method, Path: path, Body: string(respBody)}
			span.RecordError(apiErr)
			span.SetStatus(codes.Error, http.StatusText(resp.StatusCode))
			return nil, apiErr
		}
		return respBody, nil
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("backoff cancelled: %w", ctx.Err())
	case <-time.After(d):
		return nil
	}
}

func (c *Client) backoff(attempt int) time.Duration {
	base := c.backoffBase
	if base <= 0 {
		base = rateRetryBase
	}
	d := base << attempt
	if d > maxRetryAfter {
		return maxRetryAfter
	}
	return d
}

func (c *Client) retryAfterDelay(header string, attempt int) time.Duration {
	if header != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && secs >= 0 {
			d := time.Duration(secs) * time.Second
			if d > maxRetryAfter {
				return maxRetryAfter
			}
			return d
		}
	}
	return c.backoff(attempt)
}

func cloneValues(v url.Values) url.Values {
	out := make(url.Values, len(v))
	for k, vs := range v {
		out[k] = append([]string(nil), vs...)
	}
	return out
}
