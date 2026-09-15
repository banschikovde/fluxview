package kustomize

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	builder := NewBuilder(root, WithRemoteCache(filepath.Join(t.TempDir(), "remote"), time.Hour, 30*time.Second),
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

func TestBuildCache_TouchWithoutContentChangeStillHits(t *testing.T) {
	root := writeBuildFixture(t)
	builder := NewBuilder(root, WithBuildCache(filepath.Join(t.TempDir(), "builds"), time.Hour))
	appDir := filepath.Join(root, "app")

	out1, err := builder.Build(context.Background(), appDir)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}

	// Same content, new mtimes everywhere — exactly what a fresh git
	// checkout does. The entry must stay valid: content is the identity,
	// not mtime. A rebuild would panic on the nil'ed kustomizer.
	future := time.Now().Add(2 * time.Second)
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil {
			_ = os.Chtimes(p, future, future)
		}
		return nil
	})
	builder.kustomizer = nil
	out2, err := builder.Build(context.Background(), appDir)
	if err != nil {
		t.Fatalf("second build after touch-only: %v", err)
	}
	if string(out1) != string(out2) {
		t.Fatal("touch without content change must serve the cached output")
	}
}

func TestBuildCache_RebuildsOnNewFileInRecordedDir(t *testing.T) {
	root := writeBuildFixture(t)
	builder := NewBuilder(root, WithBuildCache(filepath.Join(t.TempDir(), "builds"), time.Hour))
	appDir := filepath.Join(root, "app")

	if _, err := builder.Build(context.Background(), appDir); err != nil {
		t.Fatalf("first build: %v", err)
	}

	// A file appearing in a recorded directory changes its listing.
	writeFileT(t, filepath.Join(appDir, "extra.yaml"), []byte("# stray\n"))

	kustPath := filepath.Join(appDir, "kustomization.yaml")
	kustHash, _, err := hashFileContent(kustPath)
	if err != nil {
		t.Fatal(err)
	}
	cache := cacheForBuilder(t, builder)
	entryPath := cache.entryPath(root, appDir, kustPath, kustHash)
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
	// paths; a content change must invalidate, a touch must not.
	root := writeBuildFixture(t)
	remoteFile := filepath.Join(t.TempDir(), "remote", "cached.yaml")
	if err := os.MkdirAll(filepath.Dir(remoteFile), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, remoteFile, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n"))

	hash, size, err := hashFileContent(remoteFile)
	if err != nil {
		t.Fatal(err)
	}
	entry := buildCacheEntry{
		Files: []buildFileInput{{Path: remoteFile, Size: size, SHA256: hash}},
	}
	if !verifyBuildInputs(root, entry) {
		t.Fatal("entry with untouched absolute input must verify")
	}
	// Touch only: same content, new mtime — must still verify.
	future := time.Now().Add(time.Second)
	if err := os.Chtimes(remoteFile, future, future); err != nil {
		t.Fatal(err)
	}
	if !verifyBuildInputs(root, entry) {
		t.Fatal("touch without content change must keep the entry valid")
	}
	writeFileT(t, remoteFile, []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm2\n"))
	if verifyBuildInputs(root, entry) {
		t.Fatal("entry must not verify after the absolute input content changed")
	}
}

func TestBuildCache_VerifyDeletedInput(t *testing.T) {
	root := writeBuildFixture(t)
	dep := filepath.Join(root, "app", "deployment.yaml")
	hash, size, err := hashFileContent(dep)
	if err != nil {
		t.Fatal(err)
	}
	entry := buildCacheEntry{
		Files: []buildFileInput{{Path: "app/deployment.yaml", Size: size, SHA256: hash}},
	}
	if err := os.Remove(dep); err != nil {
		t.Fatal(err)
	}
	if verifyBuildInputs(root, entry) {
		t.Fatal("entry must not verify after an input was deleted")
	}
}

func TestBuildCache_KeyTracksKustomizationContent(t *testing.T) {
	root := writeBuildFixture(t)
	cache := newBuildCache(filepath.Join(t.TempDir(), "a"), time.Hour)
	appDir := filepath.Join(root, "app")
	kust := filepath.Join(appDir, "kustomization.yaml")
	hash1, _, err := hashFileContent(kust)
	if err != nil {
		t.Fatal(err)
	}
	p1 := cache.entryPath(root, appDir, kust, hash1)
	// Same content → deterministic path, regardless of mtime.
	if p2 := cache.entryPath(root, appDir, kust, hash1); p1 != p2 {
		t.Fatal("entry path must be deterministic")
	}
	// Changed kustomization content → different entry file.
	writeFileT(t, kust, []byte(
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\n"))
	hash2, _, err := hashFileContent(kust)
	if err != nil {
		t.Fatal(err)
	}
	if p3 := cache.entryPath(root, appDir, kust, hash2); filepath.Base(p1) == filepath.Base(p3) {
		t.Fatal("entry key must change when the kustomization content changes")
	}
}

// TestBuildCache_SurvivesFreshCheckout is the CI property: a new copy of the
// tree at a different path with fresh mtimes hits the cache built from the
// original tree, because identity is content, not location or mtime.
func TestBuildCache_SurvivesFreshCheckout(t *testing.T) {
	root := writeBuildFixture(t)
	cacheDir := filepath.Join(t.TempDir(), "builds")
	builder := NewBuilder(root, WithBuildCache(cacheDir, time.Hour))
	appDir := filepath.Join(root, "app")

	out1, err := builder.Build(context.Background(), appDir)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}

	// Simulated CI checkout: copy of the tree elsewhere, all mtimes = now.
	checkout := filepath.Join(t.TempDir(), "repo-copy")
	if err := copyTree(t, root, checkout); err != nil {
		t.Fatalf("copy tree: %v", err)
	}
	now := time.Now()
	_ = filepath.Walk(checkout, func(p string, info os.FileInfo, err error) error {
		if err == nil {
			_ = os.Chtimes(p, now, now)
		}
		return nil
	})

	ci := NewBuilder(checkout, WithBuildCache(cacheDir, time.Hour))
	ci.kustomizer = nil // a cache miss would now panic
	out2, err := ci.Build(context.Background(), filepath.Join(checkout, "app"))
	if err != nil {
		t.Fatalf("fresh checkout must hit the cache: %v", err)
	}
	if string(out1) != string(out2) {
		t.Fatal("cached output differs across checkouts")
	}
}

// copyTree recursively copies src to dst preserving relative layout.
func copyTree(t *testing.T, src, dst string) error {
	t.Helper()
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
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

// TestBuildCache_RemoteRefreshInvalidatesEntry pins the interaction between
// the remote and build caches: a floating remote resource re-fetched with new
// content must invalidate a still-fresh build entry. The build-cache lookup
// runs after remote.prepare precisely so the refreshed content (new hash) is
// visible to manifest verification — doing it the other way around would
// serve a stale entry until its own TTL expired.
func TestBuildCache_RemoteRefreshInvalidatesEntry(t *testing.T) {
	var body atomic.Value
	body.Store(remoteCRDBody)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, body.Load().(string))
	}))
	t.Cleanup(srv.Close)
	url := srv.URL + "/live/crd.yaml" // no version marker → floating

	repo := t.TempDir()
	overlay := filepath.Join(repo, "overlay")
	writeRemoteKustomization(t, overlay, url)

	remoteDir := filepath.Join(t.TempDir(), "remote")
	buildsDir := filepath.Join(t.TempDir(), "builds")

	// First run: floating TTL > 0, the resource is downloaded and the build
	// output is cached.
	b1 := NewBuilder(repo, WithRemoteCache(remoteDir, time.Hour, 30*time.Second), WithBuildCache(buildsDir, time.Hour))
	out1, err := b1.Build(context.Background(), overlay)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	if !strings.Contains(string(out1), "from-remote") {
		t.Fatalf("first output missing remote resource:\n%s", out1)
	}

	// The remote content changes.
	body.Store(strings.Replace(remoteCRDBody, "name: from-remote", "name: from-remote-v2", 1))

	// Second run: remote TTL 0 forces a re-fetch; the refreshed file's new
	// content hash must invalidate the build entry even though its own TTL
	// has not expired.
	b2 := NewBuilder(repo, WithRemoteCache(remoteDir, 0, 30*time.Second), WithBuildCache(buildsDir, time.Hour))
	out2, err := b2.Build(context.Background(), overlay)
	if err != nil {
		t.Fatalf("second build: %v", err)
	}
	if !strings.Contains(string(out2), "from-remote-v2") {
		t.Fatalf("build entry was not invalidated by the remote refresh:\n%s", out2)
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
