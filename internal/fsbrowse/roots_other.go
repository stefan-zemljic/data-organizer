//go:build !windows

package fsbrowse

// Roots returns the filesystem root.
func Roots() ([]Entry, error) {
	return []Entry{{Name: "/", Path: "/", Kind: KindDir}}, nil
}
