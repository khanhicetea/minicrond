package logstore

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/khanhicetea/minicrond/internal/model"
)

// bufPath is the path under dir/logs of one of run's buffer files, given by
// kind: "index.json" (the index), "index.json.tmp" or "NNNNNN.zst" (a chunk).
func bufPath(dir, run, name string) string {
	base := filepath.Join(dir, "logs", run)
	switch name {
	case "index.json":
		return indexPath(base)
	case "index.json.tmp":
		return indexTmpPath(base)
	}
	digits, ok := strings.CutSuffix(name, ".zst")
	if !ok {
		panic("bufPath: unexpected name " + name)
	}
	n, err := strconv.Atoi(digits)
	if err != nil {
		panic("bufPath: unexpected name " + name)
	}
	if len(digits) > 6 {
		return base + "." + digits + ".zst" // already wider than the padding
	}
	return chunkPath(base, n)
}

// bufferFiles lists the flat buffer files of run under dir/logs.
func bufferFiles(t *testing.T, dir, run string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "logs"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if id, _, _, ok := bufferEntry(e.Name()); ok && id == run {
			out = append(out, e.Name())
		}
	}
	return out
}

// bufferExists reports whether any buffer file of run is on disk.
func bufferExists(t *testing.T, dir, run string) bool {
	t.Helper()
	return len(bufferFiles(t, dir, run)) > 0
}

func TestBufferEntryNames(t *testing.T) {
	for _, tc := range []struct {
		name   string
		id     string
		kind   byte
		number int
		ok     bool
	}{
		{"0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b.json", "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b", 'i', 0, true},
		{"run.json.tmp", "run", 't', 0, true},
		{"run.000001.zst", "run", 'c', 1, true},
		{"run.1000000.zst", "run", 'c', 1000000, true},
		{"run.000000.zst", "", 0, 0, false},
		{"run.abc.zst", "", 0, 0, false},
		{"run.txt", "", 0, 0, false},
		{"run", "", 0, 0, false},
		{".quarantine", "", 0, 0, false},
		{".hidden.json", "", 0, 0, false},
	} {
		id, kind, number, ok := bufferEntry(tc.name)
		if id != tc.id || kind != tc.kind || number != tc.number || ok != tc.ok {
			t.Errorf("bufferEntry(%q) = %q %q %d %v, want %q %q %d %v", tc.name, id, kind, number, ok, tc.id, tc.kind, tc.number, tc.ok)
		}
	}
}

// Run ids that cannot name flat files are refused before anything is created.
func TestOpenRejectsRunIDsThatCannotNameFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "a.b", ".hidden", "a/b", `a\b`} {
		if _, err := s.Open(id, "job", model.KindJob, WriterOptions{}); err == nil {
			t.Errorf("Open(%q) succeeded", id)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "logs")); len(entries) != 0 {
		t.Fatalf("refused run ids left files: %v", entries)
	}
}

// A run's buffer is flat files: no directory is created or removed per run,
// and the group sync fsyncs the log root once for all sealed runs.
func TestBuffersAreFlatFilesSyncedThroughTheRoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "logs")
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		w, err := s.Open(id, "job", model.KindJob, WriterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Pipe(Stdout, bytes.NewReader(numberedLines(id, 3))); err != nil {
			t.Fatal(err)
		}
		if err := s.Seal(id); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("buffer created a directory: %s", e.Name())
		}
		names = append(names, e.Name())
	}
	want := []string{"a.000001.zst", "a.json", "b.000001.zst", "b.json", "c.000001.zst", "c.json"}
	if !slices.Equal(names, want) {
		t.Fatalf("buffer files = %v, want %v", names, want)
	}
	s.syncMu.Lock()
	sealed := slices.Clone(s.sealed)
	s.syncMu.Unlock()
	if len(sealed) != 3 {
		t.Fatalf("%d deferred seals, want 3", len(sealed))
	}
	// Every seal's files sit in the root, so the pass syncs one directory.
	for _, c := range sealed {
		if filepath.Dir(c.path) != root || filepath.Dir(c.index) != root || c.dir != "" {
			t.Fatalf("seal %+v is not a flat buffer file of the root", c)
		}
	}
	s.flushDirty()
	for _, id := range []string{"a", "b", "c"} {
		checkSequence(t, readAll(t, s, id), 3)
	}
}

// bufferChunks finds every chunk of a buffer from its index: chunks newer
// than the index (crash before the next seal), the open chunk after archival
// emptied the index, and an abandoned chunk just below the indexed ones.
func TestBufferChunksFollowTheIndex(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "logs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, "run")
	write := func(n int) {
		if err := os.WriteFile(chunkPath(base, n), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	numbers := func() []int {
		files, _, err := bufferChunks(base, nil)
		if err != nil {
			t.Fatal(err)
		}
		var out []int
		for _, f := range files {
			out = append(out, f.number)
		}
		return out
	}
	setIndex := func(body string) {
		if err := os.WriteFile(indexPath(base), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if got := numbers(); len(got) != 0 {
		t.Fatalf("no buffer: chunks %v", got)
	}
	write(1)
	write(2)
	if got := numbers(); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("without an index: chunks %v, want [1 2]", got)
	}
	// Chunks 1-3 archived; the index lists none but names open chunk 5. Chunk
	// 4 was abandoned empty, chunk 6 was opened after the index was written.
	for _, n := range []int{1, 2} {
		_ = os.Remove(chunkPath(base, n))
	}
	write(4)
	write(5)
	write(6)
	setIndex(`{"version":1,"chunks":[],"final":false,"truncated":false,"open":5}`)
	if got := numbers(); !slices.Equal(got, []int{4, 5, 6}) {
		t.Fatalf("after archival: chunks %v, want [4 5 6]", got)
	}
	// A stale index naming a removed chunk is checked against the disk.
	setIndex(`{"version":1,"chunks":[{"number":3,"first":1,"last":1,"bytes":1,"raw":1}],"final":false,"truncated":false}`)
	if got := numbers(); !slices.Equal(got, []int{4, 5, 6}) {
		t.Fatalf("stale index: chunks %v, want [4 5 6]", got)
	}
	// A listing adds strays the index cannot reach.
	write(9)
	files, _, err := bufferChunks(base, []chunkFile{{number: 9, path: chunkPath(base, 9)}})
	if err != nil || len(files) != 4 || files[3].number != 9 {
		t.Fatalf("with a listed stray: %v, %v", files, err)
	}
	if err := removeRun(base, []chunkFile{{number: 9, path: chunkPath(base, 9)}}); err != nil {
		t.Fatal(err)
	}
	if left := bufferFiles(t, dir, "run"); len(left) != 0 {
		t.Fatalf("removeRun left %v", left)
	}
}

// Deleting runs that have no buffer (the usual retention case) costs a few
// failed lookups, not a directory walk.
func TestRemoveRunWithoutBuffer(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "logs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := removeRun(filepath.Join(root, "gone"), nil); err != nil {
		t.Fatal(err)
	}
	// An unremovable chunk is an error, not silently skipped.
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	base := filepath.Join(root, "kept")
	if err := os.WriteFile(chunkPath(base, 1), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(root, 0o700)
	if err := removeRun(base, nil); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("removeRun on a read-only root = %v, want EACCES", err)
	}
}
