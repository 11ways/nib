//go:build linux

package platform

import "golang.org/x/sys/unix"

// InMemory says whether the filesystem holding dir keeps its files in memory (tmpfs, ramfs); false when it cannot be told.
func InMemory(dir string) bool {
	var st unix.Statfs_t
	if unix.Statfs(dir, &st) != nil {
		return false
	}
	return st.Type == unix.TMPFS_MAGIC || st.Type == unix.RAMFS_MAGIC
}
