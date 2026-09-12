package kustomize

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeBuildFixture creates a minimal repository:
//
//	<root>/app/kustomization.yaml  (namespace transformer)
//	<root>/app/deployment.yaml
func writeBuildFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(appDir, "kustomization.yaml"), []byte(
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nnamespace: demo\nresources:\n  - deployment.yaml\n"))
	writeFileT(t, filepath.Join(appDir, "deployment.yaml"), []byte(
		"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: demo\nspec:\n  replicas: 1\n"))
	return root
}

func writeFileT(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// countCacheEntries returns the number of files in the build cache directory.
func countCacheEntries(t *testing.T, cacheDir string) int {
	t.Helper()
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestBuildCache_ServesSecondBuildWithoutKustomize(t *testing.T) {
	root := writeBuildFixture(t)
	cacheDir := filepath.Join(t.TempDir(), "builds")
	builder := NewBuilder(root, WithRemoteCache(filepath.Join(t.TempDir(), "remote"), time.Hour),
		WithBuildCache(cacheDir, time.Hour))

	out1, err := builder.Build(context.Background(), filepath.Join(root, "app"))
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	if got := countCacheEntries(t, cacheDir); got != 1 {
		t.Fatalf("expected 1 cache entry after first build, got %d", got)
	}

	// The kustomizer is only reached on a cache miss: nil it out so a miss
	// would panic instead of silently rebuilding.
	builder.kustomizer = nil
	out2, err := builder.Build(context.Background(), filepath.Join(root, "app"))
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if string(out1) != string(out2) {
		t.Fatalf("cached output differs:\n%s\n---\n%s", out1, out2)
	}
}

func TestBuildCache_RebuildsOnFileChange(t *testing.T) {
	root := writeBuildFixture(t)
	cacheDir := filepath.Join(t.TempDir(), "builds")
	builder := NewBuilder(root, WithBuildCache(cacheDir, time.Hour))
	appDir := filepath.Join(root, "app")

	out1, err := builder.Build(context.Background(), appDir)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}

	writeFileT(t, filepath.Join(appDir, "deployment.yaml"), []byte(
		"apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: demo\nspec:\n  replicas: 5\n"))
	out2, err := builder.Build(context.Background(), appDir)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if string(out1) == string(out2) {
		t.Fatal("expected a different output after editing an input file")
	}
	if !strings.Contains(string(out2), "replicas: 5") {
		t.Fatalf("rebuild output does not reflect the edit:\n%s", out2)
	}
}

func TestBuildCache_RebuildsOnMtimeTouch(t *testing.T) {
	root := writeBuildFixture(t)
	builder := NewBuilder(root, WithBuildCache(filepath.Join(t.TempDir(), "builds"), time.Hour))
	appDir := filepath.Join(root, "app")

	if _, err := builder.Build(context.Background(), appDir); err != nil {
		t.Fatalf("first build: %v", err)
	}

	// Same content, new mtime: the entry must be considered stale (safe
	// direction — mtime is the identity the cache trusts).
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(appDir, "deployment.yaml"), future, future); err != nil {
		t.Fatal(err)
	}

	// Direct lookup check: the manifest must no longer verify.
	kustPath := filepath.Join(appDir, "kustomization.yaml")
	st, err := os.Stat(kustPath)
	if err != nil {
		t.Fatal(err)
	}
	cache := cacheForBuilder(t, builder)
	entryPath := cache.entryPath(root, appDir, kustPath, st.Size(), st.ModTime().UnixNano())
	data, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatalf("entry written by first builder: %v", err)
	}
	var entry buildCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	if verifyBuildInputs(root, entry) {
		t.Fatal("manifest still verifies after touching an input file")
	}
}

func TestBuildCache_RebuildsOnNewFileInRecordedDir(t *testing.T) {
	root := writeBuildFixture(t)
	builder := NewBuilder(root, WithBuildCache(filepath.Join(t.TempDir(), "builds"), time.Hour))
	appDir := filepath.Join(root, "app")

	if _, err := builder.Build(context.Background(), appDir); err != nil {
		t.Fatalf("first build: %v", err)
	}

	// A file appearing in a recorded directory changes the directory mtime.
	writeFileT(t, filepath.Join(appDir, "extra.yaml"), []byte("# stray\n"))

	kustPath := filepath.Join(appDir, "kustomization.yaml")
	st, err := os.Stat(kustPath)
	if err != nil {
		t.Fatal(err)
	}
	cache := cacheForBuilder(t, builder)
	entryPath := cache.entryPath(root, appDir, kustPath, st.Size(), st.ModTime().UnixNano())
	data, err := os.ReadFile(entryPath)
	if err != nil {
		t.Fatalf("entry: %v", err)
	}
	var entry buildCacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	if verifyBuildInputs(root, entry) {
		t.Fatal("manifest still verifies after adding a file to the build directory")
	}
}

func TestBuildCache_TTLExpiry(t *testing.T) {
	root := writeBuildFixture(t)
	cacheDir := filepath.Join(t.TempDir(), "builds")
	builder := NewBuilder(root, WithBuildCache(cacheDir, 50*time.Millisecond))
	appDir := filepath.Join(root, "app")

	if _, err := builder.Build(context.Background(), appDir); err != nil {
		t.Fatalf("first build: %v", err)
	}
	time.Sleep(80 * time.Millisecond)

	cache := cacheForBuilder(t, builder)
	if _, ok := cache.lookup(root, appDir, filepath.Join(appDir, "kustomization.yaml")); ok {
		t.Fatal("entry served after TTL expiry")
	}
}

func TestBuildCache_CorruptEntryIsAMiss(t *testing.T) {
	root := writeBuildFixture(t)
	cacheDir := filepath.Join(t.TempDir(), "builds")
	builder := NewBuilder(root, WithBuildCache(cacheDir, time.Hour))
	appDir := filepath.Join(root, "app")

	if _, err := builder.Build(context.Background(), appDir); err != nil {
		t.Fatalf("first build: %v", err)
	}
	entries, err := os.ReadDir(cacheDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected exactly one entry, err=%v n=%d", err, len(entries))
	}
	writeFileT(t, filepath.Join(cacheDir, entries[0].Name()), []byte("{not json"))

	cache := cacheForBuilder(t, builder)
	if _, ok := cache.lookup(root, appDir, filepath.Join(appDir, "kustomization.yaml")); ok {
		t.Fatal("corrupt entry must be a miss")
	}
}

func TestBuildCache_DisabledViaDirOff(t *testing.T) {
	root := writeBuildFixture(t)
	builder := NewBuilder(root, WithBuildCache("off", time.Hour))
	if builder.buildCache != nil {
		t.Fatal("cache must be disabled with dir=off")
	}
	builder = NewBuilder(root, WithBuildCache("NONE", time.Hour))
	if builder.buildCache != nil {
		t.Fatal("cache must be disabled with dir=NONE")
	}
}

func TestBuildCache_ZeroTTLBypassesButWrites(t *testing.T) {
	root := writeBuildFixture(t)
	cacheDir := filepath.Join(t.TempDir(), "builds")
	appDir := filepath.Join(root, "app")

	// ttl=0: this process always rebuilds…
	bypass := NewBuilder(root, WithBuildCache(cacheDir, 0))
	if _, err := bypass.Build(context.Background(), appDir); err != nil {
		t.Fatalf("build with ttl=0: %v", err)
	}
	// …but entries are still written, so a later run with a non-zero ttl
	// can serve them.
	entries, err := os.ReadDir(cacheDir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 entry written despite ttl=0, err=%v n=%d", err, len(entries))
	}

	serving := NewBuilder(root, WithBuildCache(cacheDir, time.Hour))
	if _, err := serving.Build(context.Background(), appDir); err != nil {
		t.Fatalf("serving build: %v", err)
	}
	serving.kustomizer = nil // a cache miss would now panic
	if _, err := serving.Build(context.Background(), appDir); err != nil {
		t.Fatalf("second serving build must hit the cache: %v", err)
	}

	// A bypass builder never serves, even with a warm cache (a cache miss
	// reaches the nil'ed kustomizer and panics).
	bypass2 := NewBuilder(root, WithBuildCache(cacheDir, 0))
	bypass2.kustomizer = nil
	served := func() (served bool) {
		defer func() { served = recover() == nil }()
		_, err := bypass2.Build(context.Background(), appDir)
		return err == nil
	}()
	if served {
		t.Fatal("ttl=0 must always rebuild, never serve from cache")
	}
}

func TestBuildCache_VerifyAbsoluteInputsOutsideRoot(t *testing.T) {
	// Inputs outside rootDir (remote-cache files) are stored as absolute
	// paths; a mtime change must invalidate.
	root := writeBuildFixture(t)
	remoteFile := filepath.Join(t.TempDir(), "remote", "cached.yaml")
	if err := os.MkdirAll(filepath.Dir(remoteFile), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, remoteFile, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n"))

	st, err := os.Stat(remoteFile)
	if err != nil {
		t.Fatal(err)
	}
	entry := buildCacheEntry{
		Files: []buildFileInput{{Path: remoteFile, Size: st.Size(), ModTimeNano: st.ModTime().UnixNano()}},
	}
	if !verifyBuildInputs(root, entry) {
		t.Fatal("entry with untouched absolute input must verify")
	}
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(remoteFile, future, future); err != nil {
		t.Fatal(err)
	}
	if verifyBuildInputs(root, entry) {
		t.Fatal("entry must not verify after the absolute input changed")
	}
}

func TestBuildCache_VerifyDeletedInput(t *testing.T) {
	root := writeBuildFixture(t)
	dep := filepath.Join(root, "app", "deployment.yaml")
	st, err := os.Stat(dep)
	if err != nil {
		t.Fatal(err)
	}
	entry := buildCacheEntry{
		Files: []buildFileInput{{Path: "app/deployment.yaml", Size: st.Size(), ModTimeNano: st.ModTime().UnixNano()}},
	}
	if err := os.Remove(dep); err != nil {
		t.Fatal(err)
	}
	if verifyBuildInputs(root, entry) {
		t.Fatal("entry must not verify after an input was deleted")
	}
}

func TestBuildCache_KeyTracksKustomizationStat(t *testing.T) {
	root := writeBuildFixture(t)
	cache := newBuildCache(filepath.Join(t.TempDir(), "a"), time.Hour)
	appDir := filepath.Join(root, "app")
	kust := filepath.Join(appDir, "kustomization.yaml")
	st, err := os.Stat(kust)
	if err != nil {
		t.Fatal(err)
	}
	p1 := cache.entryPath(root, appDir, kust, st.Size(), st.ModTime().UnixNano())
	// Same inputs → deterministic path.
	if p2 := cache.entryPath(root, appDir, kust, st.Size(), st.ModTime().UnixNano()); p1 != p2 {
		t.Fatal("entry path must be deterministic")
	}
	// Different kustomization mtime or size → different entry file.
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(kust, future, future); err != nil {
		t.Fatal(err)
	}
	st2, err := os.Stat(kust)
	if err != nil {
		t.Fatal(err)
	}
	if p3 := cache.entryPath(root, appDir, kust, st2.Size(), st2.ModTime().UnixNano()); filepath.Base(p1) == filepath.Base(p3) {
		t.Fatal("entry key must change when the kustomization file stat changes")
	}
}

func TestBuildCache_FailedBuildNotCached(t *testing.T) {
	root := t.TempDir()
	appDir := filepath.Join(root, "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join(appDir, "kustomization.yaml"), []byte(
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - missing.yaml\n"))

	cacheDir := filepath.Join(t.TempDir(), "builds")
	builder := NewBuilder(root, WithBuildCache(cacheDir, time.Hour))
	if _, err := builder.Build(context.Background(), appDir); err == nil {
		t.Fatal("expected build failure for missing resource")
	}
	if entries, err := os.ReadDir(cacheDir); err == nil && len(entries) > 0 {
		t.Fatalf("failed build must not be cached, found %d entries", len(entries))
	}

	// Fix the resource: the next build must succeed and cache.
	writeFileT(t, filepath.Join(appDir, "missing.yaml"), []byte(
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n"))
	out, err := builder.Build(context.Background(), appDir)
	if err != nil {
		t.Fatalf("build after fix: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("empty output")
	}
	if entries, err := os.ReadDir(cacheDir); err != nil || len(entries) != 1 {
		t.Fatalf("expected 1 entry after successful build, err=%v n=%d", err, len(entries))
	}
}

// cacheForBuilder returns the builder's build cache for direct unit checks.
func cacheForBuilder(t *testing.T, b *Builder) *buildCache {
	t.Helper()
	if b.buildCache == nil {
		t.Fatal("builder has no build cache")
	}
	return b.buildCache
}
