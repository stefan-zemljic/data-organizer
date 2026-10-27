//go:build !windows && !darwin

package fsbrowse

import (
	"io/fs"
	"time"
)

// creationTime is not available through the standard library here
// (Linux needs statx); the field is left out of the response.
func creationTime(fs.FileInfo) (time.Time, bool) {
	return time.Time{}, false
}
