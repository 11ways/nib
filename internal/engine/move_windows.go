//go:build windows

package engine

import (
	"os"
	"path/filepath"
)

// moveOut renames the pinned entry to target, an absolute path elsewhere. Windows offers no rename relative to an open directory, so it
// checks again that the path still names the pinned directory and renames by path.
func (p *parent) moveOut(target string) error {
	if err := p.check(); err != nil {
		return err
	}
	return os.Rename(filepath.Join(p.directory, p.name), target)
}
