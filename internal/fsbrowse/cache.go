package fsbrowse

import (
	"io/fs"
	"os"
	"slices"
	"sync"
	"time"
)

// A folder's sorted listing is reused for its following pages: paging
// through a big folder (e.g. WinSxS) would otherwise re-read and re-sort it
// for every page. An entry is valid while the folder's modification time
// is unchanged (adding, removing or renaming entries changes it) and for
// at most cacheTTL.
const (
	cacheTTL     = 30 * time.Second
	cacheEntries = 16
)

type cachedListing struct {
	modTime time.Time
	at      time.Time
	entries []fs.DirEntry
}

var listingCache = struct {
	sync.Mutex
	m map[string]cachedListing
}{m: map[string]cachedListing{}}

// sortedEntries returns dir's eligible entries, directories first, then by
// name.
func sortedEntries(dir string, modTime time.Time) ([]fs.DirEntry, error) {
	listingCache.Lock()
	c, ok := listingCache.m[dir]
	listingCache.Unlock()
	if ok && c.modTime.Equal(modTime) && time.Since(c.at) < cacheTTL {
		return c.entries, nil
	}

	all, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	eligible := slices.DeleteFunc(all, func(de fs.DirEntry) bool {
		return !isEligible(de.Type())
	})
	slices.SortFunc(eligible, compareEntries)

	listingCache.Lock()
	defer listingCache.Unlock()
	if len(listingCache.m) >= cacheEntries {
		oldest := ""
		for k, v := range listingCache.m {
			if oldest == "" || v.at.Before(listingCache.m[oldest].at) {
				oldest = k
			}
		}
		delete(listingCache.m, oldest)
	}
	listingCache.m[dir] = cachedListing{modTime: modTime, at: time.Now(), entries: eligible}
	return eligible, nil
}
