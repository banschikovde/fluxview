package kustomize

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// sha256Hex is a test helper mirroring the manifest hash encoding.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestRecordingFs_RecordsReadsAndDirs(t *testing.T) {
	dir := t.TempDir()
	writeFileT(t, filepath.Join(dir, "a.yaml"), []byte("a: 1\n"))
	writeFileT(t, filepath.Join(dir, "b.yaml"), []byte("b: 2\n"))

	rec := newRecordingFs(filesys.MakeFsOnDisk())

	if _, err := rec.ReadFile(filepath.Join(dir, "a.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.ReadFile(filepath.Join(dir, "a.yaml")); err != nil { // dedup
		t.Fatal(err)
	}
	if _, err := rec.ReadFile(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("expected read error for missing file")
	}
	if _, _, err := rec.CleanedAbs(dir); err != nil {
		t.Fatal(err)
	}

	files, dirs, ok := rec.manifest(dir)
	if !ok {
		t.Fatal("recording must not be poisoned")
	}
	if len(files) != 1 || filepath.Base(files[0].Path) != "a.yaml" {
		t.Fatalf("expected only a.yaml recorded, got %v", files)
	}
	if len(files) == 1 {
		want := sha256Hex([]byte("a: 1\n"))
		if files[0].Size != int64(len("a: 1\n")) || files[0].SHA256 != want {
			t.Fatalf("file content hash not captured: %+v", files[0])
		}
	}
	if len(dirs) != 1 || dirs[0].Path != "." {
		t.Fatalf("expected the recording root dir recorded as '.', got %v", dirs)
	}
	if len(dirs) == 1 && !slices.Contains(dirs[0].Entries, "b.yaml") {
		t.Fatalf("dir listing must include b.yaml, got %v", dirs[0].Entries)
	}
}

// TestRecordingFs_HashReflectsServedBytes: the manifest hash is computed
// from the bytes kustomize actually consumed, not from a later disk state —
// a file changing between the build read and the manifest snapshot cannot
// mislabel the entry.
func TestRecordingFs_HashReflectsServedBytes(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.yaml")
	writeFileT(t, target, []byte("original: true\n"))

	rec := newRecordingFs(filesys.MakeFsOnDisk())
	if _, err := rec.ReadFile(target); err != nil {
		t.Fatal(err)
	}
	// Change the file AFTER the build read it.
	writeFileT(t, target, []byte("mutated: true\n"))

	files, _, ok := rec.manifest(dir)
	if !ok || len(files) != 1 {
		t.Fatalf("manifest: ok=%v files=%v", ok, files)
	}
	if want := sha256Hex([]byte("original: true\n")); files[0].SHA256 != want {
		t.Fatalf("hash must reflect the served bytes, got %s", files[0].SHA256)
	}
}

// TestRecordingFs_OpenHashesServedBytes: the same closure for the Open path —
// a file fully read through Open and then mutated on disk keeps the hash of
// the bytes that were served.
func TestRecordingFs_OpenHashesServedBytes(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "a.yaml")
	writeFileT(t, target, []byte("served: true\n"))

	rec := newRecordingFs(filesys.MakeFsOnDisk())
	f, err := rec.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// Mutate after the consumer finished reading.
	writeFileT(t, target, append(data, []byte("late mutation\n")...))

	files, _, ok := rec.manifest(dir)
	if !ok || len(files) != 1 {
		t.Fatalf("manifest: ok=%v files=%v", ok, files)
	}
	if want := sha256Hex([]byte("served: true\n")); files[0].SHA256 != want {
		t.Fatalf("hash must reflect the bytes served through Open, got %s", files[0].SHA256)
	}
}

func TestRecordingFs_RecordsGlobMatchesAndRoot(t *testing.T) {
	dir := t.TempDir()
	writeFileT(t, filepath.Join(dir, "a.yaml"), []byte("a: 1\n"))

	rec := newRecordingFs(filesys.MakeFsOnDisk())
	if _, err := rec.Glob(filepath.Join(dir, "*.yaml")); err != nil {
		t.Fatal(err)
	}

	files, dirs, ok := rec.manifest(dir)
	if !ok {
		t.Fatal("recording must not be poisoned")
	}
	if len(files) != 1 || filepath.Base(files[0].Path) != "a.yaml" {
		t.Fatalf("glob match not recorded: %v", files)
	}
	if len(dirs) != 1 || dirs[0].Path != "." {
		t.Fatalf("glob root dir not recorded: %v", dirs)
	}
}

func TestRecordingFs_GlobStaticRoot(t *testing.T) {
	cases := map[string]string{
		"a/b/*.yaml":                 "a/b",
		"a/b/c?.json":                "a/b",
		"*.yaml":                     "",
		"a/b/c.yaml":                 "", // no meta chars: nothing to guard
		filepath.Join("x", "[0-9]*"): "x",
	}
	for pattern, want := range cases {
		if got := globStaticRoot(pattern); got != want {
			t.Errorf("globStaticRoot(%q) = %q, want %q", pattern, got, want)
		}
	}
}

func TestRecordingFs_WalkRecordsEntries(t *testing.T) {
	dir := t.TempDir()
	writeFileT(t, filepath.Join(dir, "a.yaml"), []byte("a: 1\n"))

	rec := newRecordingFs(filesys.MakeFsOnDisk())
	err := rec.Walk(dir, func(path string, info os.FileInfo, err error) error { return err })
	if err != nil {
		t.Fatal(err)
	}

	files, dirs, ok := rec.manifest(dir)
	if !ok {
		t.Fatal("recording must not be poisoned")
	}
	if len(files) != 1 || filepath.Base(files[0].Path) != "a.yaml" {
		t.Fatalf("walked file not recorded: %v", files)
	}
	if len(dirs) != 1 || dirs[0].Path != "." {
		t.Fatalf("walked root not recorded: %v", dirs)
	}
}

func TestRecordingFs_PoisonedWhenDirRecordCannotBeListed(t *testing.T) {
	dir := t.TempDir()
	rec := newRecordingFs(filesys.MakeFsOnDisk())

	// A directory record that resolves to a file cannot be listed at
	// snapshot time and poisons the recording.
	writeFileT(t, filepath.Join(dir, "file.yaml"), []byte("x: 1\n"))
	rec.rememberDir(filepath.Join(dir, "file.yaml"))

	if _, _, ok := rec.manifest(dir); ok {
		t.Fatal("recording must be poisoned when a dir record cannot be listed")
	}
}
