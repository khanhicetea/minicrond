package trustedpath

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type testInfo struct {
	mode fs.FileMode
	uid  uint32
}

func (i testInfo) Name() string       { return "entry" }
func (i testInfo) Size() int64        { return 0 }
func (i testInfo) Mode() fs.FileMode  { return i.mode }
func (i testInfo) ModTime() time.Time { return time.Time{} }
func (i testInfo) IsDir() bool        { return i.mode.IsDir() }
func (i testInfo) Sys() any           { return &syscall.Stat_t{Uid: i.uid} }

func TestOwnershipAndPermissions(t *testing.T) {
	for _, tt := range []struct {
		name    string
		info    testInfo
		allowed bool
	}{
		{"root directory", testInfo{fs.ModeDir | 0755, 0}, true},
		{"root executable", testInfo{0755, 0}, true},
		{"user owned readonly", testInfo{fs.ModeDir | 0555, 1000}, false},
		{"user owned executable", testInfo{0555, 1000}, false},
		{"group writable or ACL mask", testInfo{fs.ModeDir | 0775, 0}, false},
		{"world writable", testInfo{0777, 0}, false},
		{"sticky directory", testInfo{fs.ModeDir | fs.ModeSticky | 0777, 0}, false},
		{"device", testInfo{fs.ModeDevice | 0755, 0}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := trusted(tt.info); (err == nil) != tt.allowed {
				t.Fatalf("allowed=%v err=%v", tt.allowed, err)
			}
		})
	}
}

func TestPathValidationAndSafeSymlinks(t *testing.T) {
	for _, paths := range [][]string{{}, {"."}, {"bin"}, {"/usr/bin:/bin"}, make([]string, 33)} {
		if err := Validate(paths); err == nil {
			t.Fatalf("accepted %v", paths)
		}
	}
	if err := Validate(nil); err != nil {
		t.Fatal(err)
	}
	if err := Validate([]string{"/usr/bin", "/bin"}); err != nil {
		t.Fatal(err)
	}
	path, err := LookPath("sh", []string{"/bin"})
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if path != want {
		t.Fatalf("resolved=%s want=%s", path, want)
	}
	if _, err := LookPath("../sh", []string{"/bin"}); err == nil {
		t.Fatal("relative executable accepted")
	}
}

func TestWritableSymlinkRouteRejectedEvenWithSafeTarget(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "safe-looking")
	if err := os.Symlink("/usr/bin", link); err != nil {
		t.Fatal(err)
	}
	if err := Validate([]string{link}); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("untrusted route to trusted directory accepted: %v", err)
	}
	if _, err := LookPath("sh", []string{link}); err == nil {
		t.Fatal("untrusted executable route accepted")
	}
}

func TestMissingAndNonExecutableCandidates(t *testing.T) {
	if _, err := LookPath("minicrond-deliberately-missing", []string{"/usr/bin"}); err == nil {
		t.Fatal("missing executable accepted")
	}
	if _, err := LookPath("passwd", []string{"/etc"}); err == nil {
		t.Fatal("non-executable accepted")
	}
}
