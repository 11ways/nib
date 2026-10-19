package platform

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestFoldersFollowXDGWhereSetAndTheHomeFolderElsewhere(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	homeVariable := "HOME"
	if runtime.GOOS == "windows" {
		homeVariable = "USERPROFILE"
	}
	plain := env(map[string]string{homeVariable: home})
	for got, want := range map[string]string{
		ConfigHome(plain): filepath.Join(home, ".config"),
		DataHome(plain):   filepath.Join(home, ".local", "share"),
		StateHome(plain):  filepath.Join(home, ".local", "state"),
		CacheHome(plain):  filepath.Join(home, ".cache"),
	} {
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
	xdg := filepath.Join(t.TempDir(), "data")
	if got := DataHome(env(map[string]string{homeVariable: home, "XDG_DATA_HOME": xdg})); got != xdg {
		t.Errorf("XDG_DATA_HOME: %s", got)
	}
	// A relative XDG variable is ignored, as the specification says.
	if got := DataHome(env(map[string]string{homeVariable: home, "XDG_DATA_HOME": "rel"})); got != filepath.Join(home, ".local", "share") {
		t.Errorf("relative XDG_DATA_HOME: %s", got)
	}
	if got := ConfigHome(env(nil)); got != "" {
		t.Errorf("no home: %s", got)
	}
}

func TestRawModeAndFileModeRoundTrip(t *testing.T) {
	for _, bits := range []uint32{0o644, 0o755, 0o4755, 0o2750, 0o1777, 0o600} {
		mode := FileMode(bits)
		if got := RawMode(fakeInfo{mode}); got != bits {
			t.Errorf("%o: got %o", bits, got)
		}
	}
}

type fakeInfo struct{ mode fs.FileMode }

func (f fakeInfo) Name() string       { return "f" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeInfo) Sys() any           { return nil }

func TestPrivateDirectoryIsAFolderOfTheUsersOwn(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !PrivateDirectory(dir) {
		t.Fatal("the user's own folder is not private")
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if PrivateDirectory(file) || PrivateDirectory(filepath.Join(dir, "missing")) {
		t.Fatal("a file or a missing path counted as a private folder")
	}
	if link := filepath.Join(t.TempDir(), "link"); os.Symlink(dir, link) == nil && PrivateDirectory(link) {
		t.Fatal("a link to a private folder counted as one")
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if PrivateDirectory(dir) {
			t.Fatal("a folder others may enter counted as private")
		}
	}
}

func TestLinksCountsHardLinks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	links := func() uint64 {
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			t.Fatal(err)
		}
		n, err := Links(file, info)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := links(); n != 1 {
		t.Fatalf("one link: %d", n)
	}
	if err := os.Link(path, filepath.Join(dir, "b")); err != nil {
		t.Skip("hard links unsupported here:", err)
	}
	if n := links(); n != 2 {
		t.Fatalf("two links: %d", n)
	}
}

func TestDeviceIsTheSameForFilesInOneFolder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fileInfo, _ := os.Lstat(path)
	dirInfo, _ := os.Lstat(dir)
	a, okA := Device(path, fileInfo)
	b, okB := Device(dir, dirInfo)
	if !okA || !okB || a != b {
		t.Fatalf("devices %d %v, %d %v", a, okA, b, okB)
	}
}

func TestPipeKeyIsTheSameForBothEndsOfOnePipeOnly(t *testing.T) {
	r1, w1, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close()
	defer w1.Close()
	r2, w2, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	defer w2.Close()
	k1, ok1 := PipeKey(r1)
	k2, ok2 := PipeKey(w1)
	k3, ok3 := PipeKey(r2)
	if !ok1 || !ok2 || !ok3 || k1 != k2 || k1 == k3 {
		t.Fatalf("keys %q %v, %q %v, %q %v", k1, ok1, k2, ok2, k3, ok3)
	}
	file, err := os.CreateTemp(t.TempDir(), "f")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, ok := PipeKey(file); ok {
		t.Fatal("a regular file has a pipe key")
	}
}

func TestCrossDeviceKnowsTheRenameRefusal(t *testing.T) {
	if !CrossDevice(&os.LinkError{Op: "rename", Err: syscall.EXDEV}) || CrossDevice(errors.New("other")) {
		t.Fatal("cross-device refusal not told apart")
	}
}

// TestMain lets this test binary stand in for a program that starts a child of its own and waits for it, which a group signal must end
// too; each holds the pipe it was given as standard output open until it ends.
func TestMain(m *testing.M) {
	switch os.Getenv("PLATFORM_TEST_ROLE") {
	case "parent":
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "PLATFORM_TEST_ROLE=child")
		child.Stdout = os.Stdout
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		os.Stdout.WriteString("started\n")
		_ = child.Wait()
		os.Exit(0)
	case "child":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestSignalGroupEndsAProgramAndWhatItStarted(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "PLATFORM_TEST_ROLE=parent")
	cmd.Stdout = write
	Prepare(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	write.Close()
	if err := Join(cmd); err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	defer Release(cmd.Process.Pid)
	buffer := make([]byte, 64)
	if n, _ := read.Read(buffer); string(buffer[:n]) != "started\n" {
		t.Fatalf("the program said %q", buffer[:n])
	}
	if err := SignalGroup(cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	// The pipe ends only once every process holding it has: the child too.
	ended := make(chan struct{})
	go func() {
		for {
			if _, err := read.Read(buffer); err != nil {
				close(ended)
				return
			}
		}
	}()
	select {
	case <-ended:
	case <-time.After(20 * time.Second):
		t.Fatal("the program's child outlived the group signal")
	}
}
