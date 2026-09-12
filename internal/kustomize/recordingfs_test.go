package kustomize

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/kustomize/kyaml/filesys"
)

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
	if len(files) == 1 && (files[0].Size != int64(len("a: 1\n")) || files[0].ModTimeNano == 0) {
		t.Fatalf("file stat not captured: %+v", files[0])
	}
	if len(dirs) != 1 || dirs[0].Path != "." {
		t.Fatalf("expected the recording root dir recorded as '.', got %v", dirs)
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

func TestRecordingFs_PoisonedWhenRecordedPathVanishes(t *testing.T) {
	dir := t.TempDir()
	rec := newRecordingFs(filesys.MakeFsOnDisk())

	// A directory record that resolves to a file poisons the recording.
	writeFileT(t, filepath.Join(dir, "file.yaml"), []byte("x: 1\n"))
	rec.recordDir(filepath.Join(dir, "file.yaml"))

	if _, _, ok := rec.manifest(dir); ok {
		t.Fatal("recording must be poisoned when a dir record points at a file")
	}
}
