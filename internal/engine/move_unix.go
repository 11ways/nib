//go:build unix

package engine

import "golang.org/x/sys/unix"

// moveOut renames the pinned entry to target, an absolute path elsewhere, from the pinned directory itself, so a path to it that changed
// since it was checked cannot make another file move.
func (p *parent) moveOut(target string) error {
	return unix.Renameat(int(p.self.Fd()), p.name, unix.AT_FDCWD, target)
}
