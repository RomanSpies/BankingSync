package actual

import (
	"strings"
	"testing"
	"time"
)

func TestNewHULCClient(t *testing.T) {
	h := NewHULCClient()
	if len(h.ClientID) != 16 {
		t.Errorf("expected 16-char client ID, got %d: %q", len(h.ClientID), h.ClientID)
	}
	if h.InitialCount != 0 {
		t.Errorf("expected count 0, got %d", h.InitialCount)
	}
	if !h.TS.Equal(time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("expected epoch TS, got %v", h.TS)
	}
}

func TestNewHULCClient_uniqueIDs(t *testing.T) {
	a := NewHULCClient()
	b := NewHULCClient()
	if a.ClientID == b.ClientID {
		t.Error("two new clients should have different client IDs")
	}
}

func TestHULCFromTimestamp_valid(t *testing.T) {
	ts := "2024-03-15T10:30:45.123Z-0042-ABCDEF1234567890"
	h, err := HULCFromTimestamp(ts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h.ClientID != "ABCDEF1234567890" {
		t.Errorf("clientID: got %q, want %q", h.ClientID, "ABCDEF1234567890")
	}
	if h.InitialCount != 0x42 {
		t.Errorf("count: got %d (0x%X), want 0x42", h.InitialCount, h.InitialCount)
	}
	want := time.Date(2024, 3, 15, 10, 30, 45, 123_000_000, time.UTC)
	if !h.TS.Equal(want) {
		t.Errorf("TS: got %v, want %v", h.TS, want)
	}
}

func TestHULCFromTimestamp_zeroCounter(t *testing.T) {
	ts := "2025-01-01T00:00:00.000Z-0000-AAAAAAAAAAAAAAAA"
	h, err := HULCFromTimestamp(ts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h.InitialCount != 0 {
		t.Errorf("expected count 0, got %d", h.InitialCount)
	}
}

func TestHULCFromTimestamp_invalid(t *testing.T) {
	cases := []string{
		"",
		"notavalidtimestamp",
		"2024-03-15T10:30:45.123",
		"2024-03-15T10:30:45.123Z-onlyone",
	}
	for _, tc := range cases {
		_, err := HULCFromTimestamp(tc)
		if err == nil {
			t.Errorf("expected error for %q, got nil", tc)
		}
	}
}

func TestTimestamp_format(t *testing.T) {
	h := &HULCClient{
		ClientID:     "0123456789ABCDEF",
		InitialCount: 0,
		TS:           time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	fixed := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	ts := h.Timestamp(fixed)

	if !strings.HasPrefix(ts, "2024-06-01T12:00:00.000Z-") {
		t.Errorf("unexpected prefix in %q", ts)
	}
	if !strings.HasSuffix(ts, "-0123456789ABCDEF") {
		t.Errorf("unexpected suffix in %q", ts)
	}
	if !strings.Contains(ts, "Z-0000-") {
		t.Errorf("expected counter 0000 in %q", ts)
	}
}

func TestTimestamp_incrementsCounter(t *testing.T) {
	h := NewHULCClient()
	fixed := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	ts0 := h.Timestamp(fixed)
	ts1 := h.Timestamp(fixed)
	ts2 := h.Timestamp(fixed)

	if h.InitialCount != 3 {
		t.Errorf("expected count 3 after 3 calls, got %d", h.InitialCount)
	}
	if ts0 == ts1 || ts1 == ts2 {
		t.Error("successive timestamps must differ")
	}
	if !strings.Contains(ts0, "Z-0000-") {
		t.Errorf("first timestamp should have counter 0000, got %q", ts0)
	}
	if !strings.Contains(ts1, "Z-0001-") {
		t.Errorf("second timestamp should have counter 0001, got %q", ts1)
	}
	if !strings.Contains(ts2, "Z-0002-") {
		t.Errorf("third timestamp should have counter 0002, got %q", ts2)
	}
}

func TestTimestamp_usesNowWhenNoArgument(t *testing.T) {
	h := NewHULCClient()
	before := time.Now().UTC().Format("2006")
	ts := h.Timestamp()
	if !strings.HasPrefix(ts, before) {
		t.Errorf("timestamp %q should start with current year %s", ts, before)
	}
}

func TestNullTimestamp(t *testing.T) {
	h := &HULCClient{
		ClientID:     "AAAAAAAAAAAAAAAA",
		InitialCount: 5,
		TS:           time.Now(),
	}
	ts := h.NullTimestamp()
	if !strings.HasPrefix(ts, "1970-01-01T00:00:00.000Z-") {
		t.Errorf("expected epoch prefix, got %q", ts)
	}

	if !strings.Contains(ts, "Z-0005-") {
		t.Errorf("expected counter 0005 in %q", ts)
	}
	if h.InitialCount != 6 {
		t.Errorf("expected count 6 after NullTimestamp, got %d", h.InitialCount)
	}
}

func TestSinceTimestamp_doesNotIncrement(t *testing.T) {
	h := &HULCClient{
		ClientID:     "BBBBBBBBBBBBBBBB",
		InitialCount: 7,
		TS:           time.Date(2024, 1, 2, 3, 4, 5, 678_000_000, time.UTC),
	}
	ts := h.SinceTimestamp()
	want := "2024-01-02T03:04:05.678Z-0007-BBBBBBBBBBBBBBBB"
	if ts != want {
		t.Errorf("SinceTimestamp: got %q, want %q", ts, want)
	}
	if h.InitialCount != 7 {
		t.Errorf("SinceTimestamp must not increment count, got %d", h.InitialCount)
	}
}

func TestHULC_roundtrip(t *testing.T) {
	h := NewHULCClient()
	fixed := time.Date(2025, 11, 30, 23, 59, 59, 999_000_000, time.UTC)

	original := h.SinceTimestamp()
	_ = h.Timestamp(fixed)

	ts := "2025-11-30T23:59:59.999Z-0001-" + h.ClientID
	parsed, err := HULCFromTimestamp(ts)
	if err != nil {
		t.Fatalf("roundtrip parse failed: %v", err)
	}
	if parsed.SinceTimestamp() != ts {
		t.Errorf("roundtrip mismatch: got %q, want %q", parsed.SinceTimestamp(), ts)
	}
	_ = original
}
