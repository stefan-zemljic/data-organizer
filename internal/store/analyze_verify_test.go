package store

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"
)

// TestComputeDuplicatesMatchesStored compares the Go analysis with the
// counts and ranks stored by the SQL version, on a real database (read
// only). Run with DATA_ORGANIZER_VERIFY_DB=<path to data_organizer.db>.
func TestComputeDuplicatesMatchesStored(t *testing.T) {
	path := os.Getenv("DATA_ORGANIZER_VERIFY_DB")
	if path == "" {
		t.Skip("set DATA_ORGANIZER_VERIFY_DB to run")
	}
	ctx := context.Background()
	db, err := sql.Open("sqlite", path+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Store{db: db}
	ids, err := s.backupIDs(ctx, `SELECT id FROM backups WHERE ready = 1 ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for _, backupID := range ids {
		start := time.Now()
		res, err := s.computeDuplicates(ctx, backupID)
		if err != nil {
			t.Fatal(err)
		}
		took := time.Since(start)

		rows, err := db.Query(`SELECT id, dup_count, dup_rank FROM items WHERE id >= ? AND id < ? AND backup_id = ?`,
			res.minID, res.minID+int64(len(res.count)), backupID)
		if err != nil {
			t.Fatal(err)
		}
		var checked, dups, wrong int
		for rows.Next() {
			var id int64
			var count, rank int32
			rows.Scan(&id, &count, &rank)
			i := id - res.minID
			checked++
			if count > 0 {
				dups++
			}
			if res.count[i] != count || res.rank[i] != rank {
				if wrong < 5 {
					t.Errorf("backup %d item %d: go %d/%d, stored %d/%d", backupID, id, res.count[i], res.rank[i], count, rank)
				}
				wrong++
			}
		}
		rows.Close()
		t.Logf("backup %d: %d items, %d duplicates, %d differences, computed in %v", backupID, checked, dups, wrong, took.Round(time.Millisecond))
	}
}
