package main

import (
	"testing"
)

func TestEnvOr_present(t *testing.T) {
	t.Setenv("TEST_ENVOR_KEY", "myvalue")
	if got := envOr("TEST_ENVOR_KEY", "default"); got != "myvalue" {
		t.Errorf("got %q, want myvalue", got)
	}
}

func TestEnvOr_absent(t *testing.T) {
	if got := envOr("TEST_ENVOR_ABSENT_XYZ123", "default"); got != "default" {
		t.Errorf("got %q, want default", got)
	}
}

func TestEnvInt_valid(t *testing.T) {
	t.Setenv("TEST_ENVINT_KEY", "42")
	if got := envInt("TEST_ENVINT_KEY", 0); got != 42 {
		t.Errorf("got %d, want 42", got)
	}
}

func TestEnvInt_absent(t *testing.T) {
	if got := envInt("TEST_ENVINT_ABSENT_XYZ123", 7); got != 7 {
		t.Errorf("got %d, want 7", got)
	}
}

func TestEnvInt_invalid(t *testing.T) {
	t.Setenv("TEST_ENVINT_BAD", "notanumber")
	if got := envInt("TEST_ENVINT_BAD", 5); got != 5 {
		t.Errorf("got %d, want default 5", got)
	}
}

func TestCentsToDecimal(t *testing.T) {
	cases := []struct {
		cents int64
		want  string
	}{
		{0, "0.00"},
		{100, "1.00"},
		{1, "0.01"},
		{150, "1.50"},
		{12345, "123.45"},
		{-100, "-1.00"},
		{-1, "-0.01"},
		{-150, "-1.50"},
		{-99999, "-999.99"},
	}
	for _, tc := range cases {
		got := centsToDecimal(tc.cents)
		if got != tc.want {
			t.Errorf("centsToDecimal(%d) = %q, want %q", tc.cents, got, tc.want)
		}
	}
}

func TestOwnNames_empty(t *testing.T) {
	t.Setenv("ACCOUNT_HOLDER_NAME", "")
	if got := ownNames(); len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

func TestOwnNames_single(t *testing.T) {
	t.Setenv("ACCOUNT_HOLDER_NAME", "Jane Doe")
	names := ownNames()
	if _, ok := names["jane doe"]; !ok {
		t.Errorf("expected 'jane doe' in map, got %v", names)
	}
	if len(names) != 1 {
		t.Errorf("expected 1 entry, got %d", len(names))
	}
}

func TestOwnNames_multiple(t *testing.T) {
	t.Setenv("ACCOUNT_HOLDER_NAME", "Jane Doe,Doe Jane")
	names := ownNames()
	if _, ok := names["jane doe"]; !ok {
		t.Error("expected 'jane doe'")
	}
	if _, ok := names["doe jane"]; !ok {
		t.Error("expected 'doe jane'")
	}
	if len(names) != 2 {
		t.Errorf("expected 2 entries, got %d", len(names))
	}
}

func TestOwnNames_whitespace(t *testing.T) {
	t.Setenv("ACCOUNT_HOLDER_NAME", "  John Smith  ,  ")
	names := ownNames()
	if _, ok := names["john smith"]; !ok {
		t.Errorf("expected 'john smith' after trimming, got %v", names)
	}
	if len(names) != 1 {
		t.Errorf("expected 1 entry (blank segment ignored), got %d", len(names))
	}
}
