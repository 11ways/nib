package nibash

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// sandboxed wraps a real program in bwrap: the system read-only, an empty home except the toolchain directories on PATH and those in
// NIB_SANDBOX_BIND, the workspace roots writable, a private /tmp and, unless NIB_SANDBOX_NETWORK=1, no network.
func (c *call) sandboxed(dir string, argv []string) ([]string, error) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, errors.New("the sandbox runs programs in bwrap, which is not installed")
	}
	roots, err := c.tools.WorkspaceRoots()
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	args := []string{bwrap, "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp"}
	if home != "" && home != "/" {
		args = append(args, "--tmpfs", home)
		var keep []string
		for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
			if strings.HasPrefix(entry, home+string(filepath.Separator)) {
				keep = append(keep, entry)
			}
		}
		keep = append(keep, filepath.SplitList(os.Getenv("NIB_SANDBOX_BIND"))...)
		for _, path := range keep {
			if info, err := os.Stat(path); err == nil && info.IsDir() {
				args = append(args, "--ro-bind", path, path)
			}
		}
	}
	for _, root := range roots {
		args = append(args, "--bind", root, root)
	}
	if os.Getenv("NIB_SANDBOX_NETWORK") != "1" {
		args = append(args, "--unshare-net")
	}
	args = append(args, "--die-with-parent", "--chdir", dir, "--")
	return append(args, argv...), nil
}
