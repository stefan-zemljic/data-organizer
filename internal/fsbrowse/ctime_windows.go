package fsbrowse

import (
	"io/fs"
	"syscall"
	"time"
)

func creationTime(info fs.FileInfo) (time.Time, bool) {
	attr, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(0, attr.CreationTime.Nanoseconds()), true
}
