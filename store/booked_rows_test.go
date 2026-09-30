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
