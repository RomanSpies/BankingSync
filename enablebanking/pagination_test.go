package enablebanking

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testAccountUID = "acc-1"

func txnFixture(ref, date, amount string) map[string]any {
	return map[string]any{
		"entry_reference":        ref,
		"transaction_date":       date,
		"transaction_amount":     map[string]any{"amount": amount},
		"credit_debit_indicator": "DBIT",
		"status":                 "BOOK",
	}
}

func writePage(t *testing.T, w http.ResponseWriter, txns []map[string]any, continuationKey string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"transactions":     txns,
		"continuation_key": continuationKey,
	}); err != nil {
		t.Fatalf("encode page: %v", err)
	}
}

type recordedRequest struct {
	dateFrom        string
	dateTo          string
	continuationKey string
	rawQuery        string
}

func newRecordingPagedClient(t *testing.T, pages [][]map[string]any, keys []string) (*Client, *[]recordedRequest) {
	t.Helper()
	var recorded []recordedRequest

	mux := http.NewServeMux()
	mux.HandleFunc("/accounts/"+testAccountUID+"/transactions", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		recorded = append(recorded, recordedRequest{
			dateFrom:        q.Get("date_from"),
			dateTo:          q.Get("date_to"),
			continuationKey: q.Get("continuation_key"),
			rawQuery:        r.URL.RawQuery,
		})

		idx := 0
		if ck := q.Get("continuation_key"); ck != "" {
			found := false
			for i, k := range keys {
				if k == ck {
					idx = i + 1
					found = true
					break
				}
			}
			if !found {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"UNKNOWN_CONTINUATION_KEY"}`))
				return
			}
		}

		nextKey := ""
		if idx < len(keys) {
			nextKey = keys[idx]
		}
		writePage(t, w, pages[idx], nextKey)
	})

	return newTestClientWith(t, mux), &recorded
}

func TestFetchTransactions_singlePage(t *testing.T) {
	pages := [][]map[string]any{{
		txnFixture("ref-1", "2026-07-01", "10.00"),
		txnFixture("ref-2", "2026-07-02", "20.00"),
	}}
	c, recorded := newRecordingPagedClient(t, pages, nil)

	txns, _, err := c.FetchTransactions(context.Background(), testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FetchTransactions: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("got %d transactions, want 2", len(txns))
	}
	if len(*recorded) != 1 {
		t.Fatalf("got %d requests, want 1", len(*recorded))
	}
	if got := (*recorded)[0].dateFrom; got != "2026-06-15" {
		t.Errorf("date_from: got %q, want 2026-06-15", got)
	}
	if got := (*recorded)[0].dateTo; got != time.Now().UTC().Format("2006-01-02") {
		t.Errorf("date_to: got %q, want today in UTC", got)
	}
}

func TestFetchTransactions_multiPage_repeatsDateParams(t *testing.T) {
	pages := [][]map[string]any{
		{txnFixture("ref-1", "2026-07-01", "10.00")},
		{txnFixture("ref-2", "2026-07-02", "20.00")},
		{txnFixture("ref-3", "2026-07-03", "30.00")},
	}
	c, recorded := newRecordingPagedClient(t, pages, []string{"key-page-2", "key-page-3"})

	txns, _, err := c.FetchTransactions(context.Background(), testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FetchTransactions: %v", err)
	}
	if len(txns) != 3 {
		t.Fatalf("got %d transactions, want 3", len(txns))
	}
	if len(*recorded) != 3 {
		t.Fatalf("got %d requests, want 3", len(*recorded))
	}

	first := (*recorded)[0]
	for i, req := range *recorded {
		if req.dateFrom != first.dateFrom {
			t.Errorf("request %d date_from: got %q, want %q", i, req.dateFrom, first.dateFrom)
		}
		if req.dateTo != first.dateTo {
			t.Errorf("request %d date_to: got %q, want %q", i, req.dateTo, first.dateTo)
		}
	}
	if (*recorded)[0].continuationKey != "" {
		t.Errorf("first request carried a continuation_key: %q", (*recorded)[0].continuationKey)
	}
	if got := (*recorded)[1].continuationKey; got != "key-page-2" {
		t.Errorf("second request continuation_key: got %q, want key-page-2", got)
	}
	if got := (*recorded)[2].continuationKey; got != "key-page-3" {
		t.Errorf("third request continuation_key: got %q, want key-page-3", got)
	}
}

func TestFetchTransactions_continuationRequestNeverOmitsDateParams(t *testing.T) {
	pages := [][]map[string]any{
		{txnFixture("ref-1", "2026-07-01", "10.00")},
		{txnFixture("ref-2", "2026-07-02", "20.00")},
	}
	c, recorded := newRecordingPagedClient(t, pages, []string{"key-page-2"})

	if _, _, err := c.FetchTransactions(context.Background(), testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("FetchTransactions: %v", err)
	}
	// Two pages means two requests, and the continuation one is the whole point:
	// a client that stopped after the first would satisfy every assertion below
	// without ever having sent the request this test is named for.
	if len(*recorded) != 2 {
		t.Fatalf("%d requests recorded, want 2 — the continuation request is the one whose "+
			"date parameters are at issue", len(*recorded))
	}
	for i, req := range *recorded {
		if req.dateFrom == "" {
			t.Errorf("request %d omitted date_from (query %q)", i, req.rawQuery)
		}
		if req.dateTo == "" {
			t.Errorf("request %d omitted date_to (query %q)", i, req.rawQuery)
		}
	}
}

func TestFetchTransactions_rejectsWhenServerValidatesContinuationParams(t *testing.T) {
	var pageCount int
	mux := http.NewServeMux()
	mux.HandleFunc("/accounts/"+testAccountUID+"/transactions", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if ck := q.Get("continuation_key"); ck != "" {
			if q.Get("date_from") != "2026-06-15" || q.Get("date_to") != time.Now().UTC().Format("2006-01-02") {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"code":422,` +
					`"message":"dateFrom in request is not the same as in continuationKey. ` +
					`Continuation key is only valid for the same getAccountTransactions parameters",` +
					`"error":"WRONG_REQUEST_PARAMETERS"}`))
				return
			}
			pageCount++
			writePage(t, w, []map[string]any{txnFixture("ref-2", "2026-07-02", "20.00")}, "")
			return
		}
		pageCount++
		writePage(t, w, []map[string]any{txnFixture("ref-1", "2026-07-01", "10.00")}, "key-page-2")
	})
	c := newTestClientWith(t, mux)

	txns, _, err := c.FetchTransactions(context.Background(), testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FetchTransactions: %v", err)
	}
	if pageCount != 2 {
		t.Errorf("served %d pages, want 2", pageCount)
	}
	if len(txns) != 2 {
		t.Fatalf("got %d transactions, want 2", len(txns))
	}
}

func TestFetchTransactions_escapesContinuationKey(t *testing.T) {
	const trickyKey = "a+b/c=d e&f?g"
	var gotKey string
	var served int

	mux := http.NewServeMux()
	mux.HandleFunc("/accounts/"+testAccountUID+"/transactions", func(w http.ResponseWriter, r *http.Request) {
		served++
		if ck := r.URL.Query().Get("continuation_key"); ck != "" {
			gotKey = ck
			writePage(t, w, []map[string]any{txnFixture("ref-2", "2026-07-02", "20.00")}, "")
			return
		}
		writePage(t, w, []map[string]any{txnFixture("ref-1", "2026-07-01", "10.00")}, trickyKey)
	})
	c := newTestClientWith(t, mux)

	if _, _, err := c.FetchTransactions(context.Background(), testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("FetchTransactions: %v", err)
	}
	if served != 2 {
		t.Fatalf("served %d requests, want 2", served)
	}
	if gotKey != trickyKey {
		t.Errorf("continuation_key round trip: got %q, want %q", gotKey, trickyKey)
	}
}

func TestFetchTransactions_continuationKeyIsEncodedInQuery(t *testing.T) {
	const trickyKey = "a+b c"
	var rawQuery string

	mux := http.NewServeMux()
	mux.HandleFunc("/accounts/"+testAccountUID+"/transactions", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("continuation_key") != "" {
			rawQuery = r.URL.RawQuery
			writePage(t, w, nil, "")
			return
		}
		writePage(t, w, []map[string]any{txnFixture("ref-1", "2026-07-01", "10.00")}, trickyKey)
	})
	c := newTestClientWith(t, mux)

	if _, _, err := c.FetchTransactions(context.Background(), testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("FetchTransactions: %v", err)
	}
	if strings.Contains(rawQuery, "continuation_key="+trickyKey) {
		t.Errorf("continuation key was not escaped in query: %q", rawQuery)
	}
	if got := url.QueryEscape(trickyKey); !strings.Contains(rawQuery, "continuation_key="+got) {
		t.Errorf("query %q does not contain escaped key %q", rawQuery, got)
	}
}

func TestFetchTransactions_accumulatesAcrossManyPages(t *testing.T) {
	const pageCount = 5
	pages := make([][]map[string]any, pageCount)
	keys := make([]string, pageCount-1)
	for i := range pages {
		pages[i] = []map[string]any{
			txnFixture(fmt.Sprintf("ref-%d", i), fmt.Sprintf("2026-07-%02d", i+1), "10.00"),
		}
		if i < pageCount-1 {
			keys[i] = fmt.Sprintf("key-%d", i+1)
		}
	}
	c, recorded := newRecordingPagedClient(t, pages, keys)

	txns, _, err := c.FetchTransactions(context.Background(), testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FetchTransactions: %v", err)
	}
	if len(txns) != pageCount {
		t.Fatalf("got %d transactions, want %d", len(txns), pageCount)
	}
	if len(*recorded) != pageCount {
		t.Fatalf("got %d requests, want %d", len(*recorded), pageCount)
	}
	for i, txn := range txns {
		want := fmt.Sprintf("ref-%d", i)
		if txn.EntryRef != want {
			t.Errorf("transaction %d ref: got %q, want %q", i, txn.EntryRef, want)
		}
	}
}

func TestFetchTransactions_propagatesPageError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/accounts/"+testAccountUID+"/transactions", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("continuation_key") != "" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"code":422,"error":"WRONG_REQUEST_PARAMETERS"}`))
			return
		}
		writePage(t, w, []map[string]any{txnFixture("ref-1", "2026-07-01", "10.00")}, "key-page-2")
	})
	c := newTestClientWith(t, mux)

	_, _, err := c.FetchTransactions(context.Background(), testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("expected an error from the second page")
	}
	if !strings.Contains(err.Error(), "422") {
		t.Errorf("error %q does not mention 422", err.Error())
	}
}

func TestFetchTransactions_skipsMalformedButKeepsPaging(t *testing.T) {
	pages := [][]map[string]any{
		{
			txnFixture("ref-1", "2026-07-01", "10.00"),
			{"entry_reference": "bad", "transaction_amount": map[string]any{"amount": "1.00"}},
		},
		{txnFixture("ref-2", "2026-07-02", "20.00")},
	}
	c, recorded := newRecordingPagedClient(t, pages, []string{"key-page-2"})

	txns, _, err := c.FetchTransactions(context.Background(), testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FetchTransactions: %v", err)
	}
	if len(txns) != 2 {
		t.Fatalf("got %d transactions, want 2", len(txns))
	}
	if len(*recorded) != 2 {
		t.Fatalf("got %d requests, want 2", len(*recorded))
	}
}

func TestFetchTransactions_countsDroppedRecords(t *testing.T) {
	pages := [][]map[string]any{{
		txnFixture("ref-1", "2026-07-01", "10.00"),
		{"entry_reference": "no-date", "transaction_amount": map[string]any{"amount": "1.00"}},
		{"entry_reference": "bad-amount", "transaction_date": "2026-07-02", "transaction_amount": map[string]any{"amount": "abc"}},
		txnFixture("ref-2", "2026-07-03", "20.00"),
	}}
	c, _ := newRecordingPagedClient(t, pages, nil)

	txns, dropped, err := c.FetchTransactions(context.Background(), testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FetchTransactions: %v", err)
	}
	if len(txns) != 2 {
		t.Errorf("got %d parsed transactions, want 2", len(txns))
	}
	if dropped != 2 {
		t.Errorf("got %d dropped, want 2", dropped)
	}
}

func TestFetchTransactions_zeroDroppedWhenAllValid(t *testing.T) {
	pages := [][]map[string]any{{
		txnFixture("ref-1", "2026-07-01", "10.00"),
		txnFixture("ref-2", "2026-07-02", "20.00"),
	}}
	c, _ := newRecordingPagedClient(t, pages, nil)

	txns, dropped, err := c.FetchTransactions(context.Background(), testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FetchTransactions: %v", err)
	}
	if dropped != 0 {
		t.Errorf("got %d dropped, want 0", dropped)
	}
	if len(txns) != 2 {
		t.Errorf("got %d transactions, want 2", len(txns))
	}
}

func TestFetchTransactions_countsDroppedAcrossPages(t *testing.T) {
	pages := [][]map[string]any{
		{
			txnFixture("ref-1", "2026-07-01", "10.00"),
			{"entry_reference": "bad-1"},
		},
		{
			{"entry_reference": "bad-2"},
			{"entry_reference": "bad-3"},
			txnFixture("ref-2", "2026-07-02", "20.00"),
		},
	}
	c, _ := newRecordingPagedClient(t, pages, []string{"key-page-2"})

	txns, dropped, err := c.FetchTransactions(context.Background(), testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FetchTransactions: %v", err)
	}
	if len(txns) != 2 {
		t.Errorf("got %d parsed transactions, want 2", len(txns))
	}
	if dropped != 3 {
		t.Errorf("got %d dropped, want 3", dropped)
	}
}

func TestFetchTransactions_droppedCountRetainedOnPageError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/accounts/"+testAccountUID+"/transactions", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("continuation_key") != "" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"BOOM"}`))
			return
		}
		writePage(t, w, []map[string]any{
			txnFixture("ref-1", "2026-07-01", "10.00"),
			{"entry_reference": "bad-1"},
		}, "key-page-2")
	})
	c := newTestClientWith(t, mux)

	_, dropped, err := c.FetchTransactions(context.Background(), testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("expected an error from the second page")
	}
	if dropped != 1 {
		t.Errorf("got %d dropped, want 1", dropped)
	}
}

func TestSortedKeys(t *testing.T) {
	got := sortedKeys(map[string]any{"zeta": 1, "alpha": 2, "mid": 3})
	want := []string{"alpha", "mid", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestSortedKeys_empty(t *testing.T) {
	if got := sortedKeys(map[string]any{}); len(got) != 0 {
		t.Errorf("got %v, want empty", got)
	}
}

func TestFetchTransactions_repeatedContinuationKeyAborts(t *testing.T) {
	var served int
	mux := http.NewServeMux()
	mux.HandleFunc("/accounts/"+testAccountUID+"/transactions", func(w http.ResponseWriter, r *http.Request) {
		served++
		writePage(t, w, []map[string]any{txnFixture("ref-loop", "2026-07-01", "10.00")}, "same-key-forever")
	})
	c := newTestClientWith(t, mux)

	_, _, err := c.FetchTransactions(context.Background(), testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("a repeated continuation key must abort, not loop forever")
	}
	if !strings.Contains(err.Error(), "repeated continuation key") {
		t.Errorf("error %q does not mention the repeated key", err.Error())
	}
	if served > 3 {
		t.Errorf("served %d requests before aborting, want at most 3", served)
	}
}

func TestFetchTransactions_pageLimitAborts(t *testing.T) {
	var served int
	mux := http.NewServeMux()
	mux.HandleFunc("/accounts/"+testAccountUID+"/transactions", func(w http.ResponseWriter, r *http.Request) {
		served++
		writePage(t, w, []map[string]any{txnFixture(fmt.Sprintf("ref-%d", served), "2026-07-01", "10.00")},
			fmt.Sprintf("key-%d", served))
	})
	c := newTestClientWith(t, mux)

	_, _, err := c.FetchTransactions(context.Background(), testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("an endless stream of fresh continuation keys must hit the page limit")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("error %q does not mention the page limit", err.Error())
	}
	if served > maxPages+1 {
		t.Errorf("served %d pages, want at most %d", served, maxPages+1)
	}
}

func TestFetchTransactions_cancelledContextStopsPaging(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var served int
	mux := http.NewServeMux()
	mux.HandleFunc("/accounts/"+testAccountUID+"/transactions", func(w http.ResponseWriter, r *http.Request) {
		served++
		cancel()
		writePage(t, w, []map[string]any{txnFixture(fmt.Sprintf("ref-%d", served), "2026-07-01", "10.00")},
			fmt.Sprintf("key-%d", served))
	})
	c := newTestClientWith(t, mux)

	_, _, err := c.FetchTransactions(ctx, testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("a cancelled context must stop pagination")
	}
	if served > 2 {
		t.Errorf("served %d pages after cancellation, want at most 2", served)
	}
}

func TestFetchTransactions_retriesOnRateLimit(t *testing.T) {
	var attempts int
	mux := http.NewServeMux()
	mux.HandleFunc("/accounts/"+testAccountUID+"/transactions", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		writePage(t, w, []map[string]any{txnFixture("ref-1", "2026-07-01", "10.00")}, "")
	})
	c := newTestClientWith(t, mux)

	txns, _, err := c.FetchTransactions(context.Background(), testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("a 429 followed by a success must not fail the fetch: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts: got %d, want 2", attempts)
	}
	if len(txns) != 1 {
		t.Errorf("got %d transactions, want 1", len(txns))
	}
}

func TestFetchTransactions_givesUpAfterRepeatedRateLimits(t *testing.T) {
	var attempts int
	mux := http.NewServeMux()
	mux.HandleFunc("/accounts/"+testAccountUID+"/transactions", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c := newTestClientWith(t, mux)

	_, _, err := c.FetchTransactions(context.Background(), testAccountUID, time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC))
	if err == nil {
		t.Fatal("persistent rate limiting must surface as an error")
	}
	if attempts != maxRateRetry+1 {
		t.Errorf("attempts: got %d, want %d", attempts, maxRateRetry+1)
	}
}

func TestRetryAfterDelay(t *testing.T) {
	if got := retryAfterDelay("5", 0); got != 5*time.Second {
		t.Errorf("Retry-After 5: got %v, want 5s", got)
	}
	if got := retryAfterDelay("9999", 0); got != maxRetryAfter {
		t.Errorf("oversized Retry-After: got %v, want it capped at %v", got, maxRetryAfter)
	}
	if got := retryAfterDelay("", 0); got != rateRetryBase {
		t.Errorf("no header, attempt 0: got %v, want %v", got, rateRetryBase)
	}
	if got := retryAfterDelay("", 1); got != 2*rateRetryBase {
		t.Errorf("no header, attempt 1: got %v, want %v", got, 2*rateRetryBase)
	}
	if got := retryAfterDelay("not-a-number", 0); got != rateRetryBase {
		t.Errorf("malformed header: got %v, want the backoff default %v", got, rateRetryBase)
	}
}
