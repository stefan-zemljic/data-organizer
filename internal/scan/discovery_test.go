package scan

import (
	"os"
	"testing"
	"time"
)

// TestDiscoveryTime measures how long finding everything below a real
// root takes (the scan's first phase; no files are hashed). Run with
// DATA_ORGANIZER_DISCOVERY_ROOT=C:\ and a short -timeout.
func TestDiscoveryTime(t *testing.T) {
	root := os.Getenv("DATA_ORGANIZER_DISCOVERY_ROOT")
	if root == "" {
		t.Skip("set DATA_ORGANIZER_DISCOVERY_ROOT to run")
	}
	roots, err := NormalizeRoots([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan Item, 1024)
	go func() {
		for range out {
		}
	}()
	start := time.Now()
	s := New(roots, 1, out, Options{})
	s.Start()
	defer func() {
		s.Stop()
		s.Wait()
	}()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for s.Progress().Discovering {
		select {
		case <-tick.C:
			p := s.Progress()
			t.Logf("%v: %d dirs (%d done), %d files, %.1f GB, %d errors",
				time.Since(start).Round(time.Second), p.DirsFound, p.DirsDone, p.FilesFound, float64(p.BytesFound)/1e9, p.Errors)
		case <-time.After(10 * time.Millisecond):
		}
	}
	p := s.Progress()
	t.Logf("discovered %d dirs, %d files, %.1f GB in %v (%d errors)",
		p.DirsFound, p.FilesFound, float64(p.BytesFound)/1e9, time.Since(start), p.Errors)
}
