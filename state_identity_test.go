package main

import (
	"testing"
	"time"
)

func TestState_aRowIsSealedOnlyOnceItsRecordsAreOutOfReach(t *testing.T) {
	s := &State{Identities: map[int64]map[string]string{
		1: {"2026-09-25|aaaa|1": "row", "2026-09-20|bbbb|1": "row"},
	}}
	day := func(d string) time.Time { v, _ := time.Parse("2006-01-02", d); return v }

	if !s.Sealed(1, day("2026-09-26"))("row", "2026-09-28|cccc|1") {
		t.Error("a row whose records all predate the feed is not sealed against a different record")
	}
	if s.Sealed(1, day("2026-09-25"))("row", "2026-09-28|cccc|1") {
		t.Error("a row with a record still inside the feed was sealed; if that record drifted, sealing would duplicate it")
	}
	if s.Sealed(1, day("2026-09-26"))("row", "2026-09-25|aaaa|1") {
		t.Error("a row was sealed against one of its own records")
	}
	if s.Sealed(1, day("2026-09-26"))("unknown", "2026-09-28|cccc|1") {
		t.Error("a row with no recorded identity was sealed; rows from before the upgrade must behave as they did")
	}
	if s.Sealed(2, day("2026-09-26"))("row", "2026-09-28|cccc|1") {
		t.Error("another account's identities sealed this account's row")
	}
}
