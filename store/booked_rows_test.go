package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

func TestStore_bookedRowsAgeByWhenTheyWereRecordedNotByTheirDate(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	_ = st.AddBookedRow(1, "old", "2099-01-01|1.00|future|1")
	_ = st.AddBookedRow(1, "recent", "2099-01-01|1.00|future|2")
	if _, err := st.db.Exec(
		"UPDATE booked_rows SET recorded_at = datetime('now', ?) WHERE txn_id = 'old'",
		fmt.Sprintf("-%d days", RetentionDays+1),
	); err != nil {
		t.Fatalf("age the row: %v", err)
	}

	rows, err := st.PruneBookedRows()
	if err != nil {
		t.Fatalf("PruneBookedRows: %v", err)
	}
	if _, ok := rows[1]["old"]; ok {
		t.Error("a row recorded past the retention window survived because its key names a future date")
	}
	if _, ok := rows[1]["recent"]; !ok {
		t.Error("a row recorded inside the retention window was pruned")
	}
}

func TestStore_bookingIdentitiesAgeByWhenTheyWereLastSeen(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	_ = st.AddBookingIdentity(1, "2026-08-01|aaaa|1", "row-a")
	_ = st.AddBookingIdentity(1, "2026-08-01|bbbb|1", "row-b")
	if _, err := st.db.Exec(
		"UPDATE booking_identities SET recorded_at = datetime('now', ?)",
		fmt.Sprintf("-%d days", RetentionDays+1),
	); err != nil {
		t.Fatalf("age the rows: %v", err)
	}
	_ = st.AddBookingIdentity(1, "2026-08-01|bbbb|1", "row-b")

	got, err := st.PruneBookingIdentities()
	if err != nil {
		t.Fatalf("PruneBookingIdentities: %v", err)
	}
	if _, ok := got[1]["2026-08-01|aaaa|1"]; ok {
		t.Error("an identity nobody has seen for longer than the retention window survived")
	}
	if got[1]["2026-08-01|bbbb|1"] != "row-b" {
		t.Error("an identity seen again was pruned; each sighting must renew it while the bank keeps delivering the record")
	}
}
