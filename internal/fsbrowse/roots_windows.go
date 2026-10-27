package fsbrowse

import "syscall"

var procGetLogicalDrives = syscall.NewLazyDLL("kernel32.dll").NewProc("GetLogicalDrives")

// Roots returns the drive letters present on the system. Drives are not
// stat'ed, so disconnected network drives cannot stall the call.
func Roots() ([]Entry, error) {
	mask, _, err := procGetLogicalDrives.Call()
	if mask == 0 {
		return nil, err
	}
	var roots []Entry
	for i := range 26 {
		if mask&(1<<i) == 0 {
			continue
		}
		letter := string(rune('A' + i))
		roots = append(roots, Entry{Name: letter + ":", Path: letter + `:\`, Kind: KindDir})
	}
	return roots, nil
}
