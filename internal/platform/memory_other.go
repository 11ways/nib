//go:build !linux

package platform

// InMemory is false: only Linux tells nib a memory filesystem from another.
func InMemory(string) bool { return false }
