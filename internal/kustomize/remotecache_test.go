package kustomize

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// remoteCRDBody is a multi-document remote resource, mimicking real CRD bundles.
const remoteCRDBody = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: crontabs.stable.example.com
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: from-remote
`

// countingServer serves one path with a fixed body, counting GETs.
type countingServer struct {
	*httptest.Server
	mu     sync.Mutex
	gets   int
	closed bool
}

func newCountingServer(t *testing.T, path, body string) *countingServer {
	t.Helper()
	cs := &countingServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		cs.mu.Lock()
		cs.gets++
		cs.mu.Unlock()
		fmt.Fprint(w, body)
	}))
	t.Cleanup(cs.close)
	return cs
}

func (cs *countingServer) getsCount() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.gets
}

func (cs *countingServer) close() {
	cs.mu.Lock()
	if cs.closed {
		cs.mu.Unlock()
		return
	}
	cs.closed = true
	cs.mu.Unlock()
	cs.Server.Close()
}

// captureStderr runs fn with os.Stderr redirected to a buffer and returns
// everything written during that window.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = orig }()
	fn()
	w.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatalf("reading stderr: %v", err)
	}
	return buf.String()
}

// writeRemoteKustomization writes a kustomization.yaml referencing remoteURL.
func writeRemoteKustomization(t *testing.T, dir, remoteURL string) {
	t.Helper()
	writeTestFile(t, filepath.Join(dir, "kustomization.yaml"), fmt.Sprintf(
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - %s\n", remoteURL))
}

// TestRemoteCache_ColdWarmCounters verifies the core cache economics: a cold
// build performs exactly one GET, subsequent builds (fresh cache instances,
// same on-disk cache) perform zero — including with the server gone, which
// makes the test falsifiable: any network attempt would fail the build.
func TestRemoteCache_ColdWarmCounters(t *testing.T) {
	cs := newCountingServer(t, "/manifests/main/crd.yaml", remoteCRDBody)
	url := cs.URL + "/manifests/main/crd.yaml"
	repo := t.TempDir()
	overlay := filepath.Join(repo, "clusters", "test", "infra")
	writeRemoteKustomization(t, overlay, url)
	cacheDir := t.TempDir()

	// Cold: exactly one GET.
	b1 := NewBuilder(repo, WithRemoteCache(cacheDir, time.Hour, 30*time.Second))
	out1, err := b1.Build(context.Background(), overlay)
	if err != nil {
		t.Fatalf("cold build: %v", err)
	}
	if !strings.Contains(string(out1), "from-remote") {
		t.Fatalf("cold build output missing remote resource:\n%s", out1)
	}
	if n := cs.getsCount(); n != 1 {
		t.Fatalf("cold build: got %d GETs, want 1", n)
	}

	// Warm (new process simulation: new Builder, same cache dir): zero GETs.
	b2 := NewBuilder(repo, WithRemoteCache(cacheDir, time.Hour, 30*time.Second))
	out2, err := b2.Build(context.Background(), overlay)
	if err != nil {
		t.Fatalf("warm build: %v", err)
	}
	if n := cs.getsCount(); n != 1 {
		t.Fatalf("warm build: got %d GETs total, want 1 (no re-fetch)", n)
	}
	if !bytes.Equal(out1, out2) {
		t.Fatalf("warm output differs from cold output:\ncold:\n%s\nwarm:\n%s", out1, out2)
	}

	// Warm offline: server gone, build still succeeds byte-identically.
	cs.close()
	b3 := NewBuilder(repo, WithRemoteCache(cacheDir, time.Hour, 30*time.Second))
	out3, err := b3.Build(context.Background(), overlay)
	if err != nil {
		t.Fatalf("offline warm build: %v", err)
	}
	if !bytes.Equal(out1, out3) {
		t.Fatalf("offline warm output differs from online warm output")
	}
}

// TestRemoteCache_ByteIdenticalToDirectRemoteBuild proves the substitution
// preserves build output exactly: a build where kustomize fetches the URL
// itself (no cache) equals a build served from the cache — byte for byte —
// and equals a plain local-file build.
func TestRemoteCache_ByteIdenticalToDirectRemoteBuild(t *testing.T) {
	cs := newCountingServer(t, "/crds/v1.0.0/bundle.yaml", remoteCRDBody)
	url := cs.URL + "/crds/v1.0.0/bundle.yaml"
	cacheDir := t.TempDir()

	// Repo without the cache: kustomize GETs the URL itself.
	plain := t.TempDir()
	writeRemoteKustomization(t, plain, url)
	plainOut, err := NewBuilder(plain).Build(context.Background(), plain)
	if err != nil {
		t.Fatalf("plain remote build: %v", err)
	}

	// Repo with the cache: URL rewritten to the cache path.
	cached := t.TempDir()
	writeRemoteKustomization(t, cached, url)
	b := NewBuilder(cached, WithRemoteCache(cacheDir, time.Hour, 30*time.Second))
	cachedOut, err := b.Build(context.Background(), cached)
	if err != nil {
		t.Fatalf("cached build: %v", err)
	}
	if !bytes.Equal(plainOut, cachedOut) {
		t.Fatalf("cached build differs from plain remote build:\nplain:\n%s\ncached:\n%s", plainOut, cachedOut)
	}
	if n := cs.getsCount(); n != 2 {
		t.Fatalf("expected 2 GETs (kustomize's own + cache download), got %d", n)
	}

	// Local-file build (the end state of the substitution) is identical too.
	local := t.TempDir()
	writeTestFile(t, filepath.Join(local, "bundle.yaml"), remoteCRDBody)
	writeTestFile(t, filepath.Join(local, "kustomization.yaml"),
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - bundle.yaml\n")
	localOut, err := NewBuilder(local).Build(context.Background(), local)
	if err != nil {
		t.Fatalf("local build: %v", err)
	}
	if !bytes.Equal(plainOut, localOut) {
		t.Fatalf("local build differs from plain remote build:\nplain:\n%s\nlocal:\n%s", plainOut, localOut)
	}
}

// TestRemoteCache_PinnedIgnoresTTL: version-pinned URLs are served from the
// cache without any freshness check — even TTL=0 and an ancient mtime.
func TestRemoteCache_PinnedIgnoresTTL(t *testing.T) {
	cs := newCountingServer(t, "/manifests/crds.yaml", remoteCRDBody)
	url := cs.URL + "/manifests/crds.yaml?ref=v1.2.3" // ?ref=version → pinned
	repo := t.TempDir()
	overlay := filepath.Join(repo, "crds")
	writeRemoteKustomization(t, overlay, url)
	cacheDir := t.TempDir()

	b := NewBuilder(repo, WithRemoteCache(cacheDir, 0, 30*time.Second))
	if _, err := b.Build(context.Background(), overlay); err != nil {
		t.Fatalf("cold build: %v", err)
	}
	if n := cs.getsCount(); n != 1 {
		t.Fatalf("cold: %d GETs, want 1", n)
	}

	// Age the cache entry far beyond any TTL.
	rc := newRemoteCache(cacheDir, 0, 30*time.Second)
	path := rc.cachePath(url)
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	b2 := NewBuilder(repo, WithRemoteCache(cacheDir, 0, 30*time.Second))
	if _, err := b2.Build(context.Background(), overlay); err != nil {
		t.Fatalf("pinned warm build: %v", err)
	}
	if n := cs.getsCount(); n != 1 {
		t.Fatalf("pinned warm: %d GETs total, want 1 (no TTL re-fetch)", n)
	}
}

// TestRemoteCache_FloatingTTLZeroRefetches: with TTL=0 every build re-fetches
// floating refs.
func TestRemoteCache_FloatingTTLZeroRefetches(t *testing.T) {
	cs := newCountingServer(t, "/manifests/main/crd.yaml", remoteCRDBody)
	url := cs.URL + "/manifests/main/crd.yaml"
	repo := t.TempDir()
	overlay := filepath.Join(repo, "crds")
	writeRemoteKustomization(t, overlay, url)
	cacheDir := t.TempDir()

	for i := 1; i <= 2; i++ {
		b := NewBuilder(repo, WithRemoteCache(cacheDir, 0, 30*time.Second))
		if _, err := b.Build(context.Background(), overlay); err != nil {
			t.Fatalf("build %d: %v", i, err)
		}
		if n := cs.getsCount(); n != i {
			t.Fatalf("after build %d: %d GETs, want %d (TTL=0 always re-fetches)", i, n, i)
		}
	}
}

// TestRemoteCache_FloatingExpiredRefetches: an entry older than the TTL is
// re-downloaded.
func TestRemoteCache_FloatingExpiredRefetches(t *testing.T) {
	cs := newCountingServer(t, "/manifests/main/crd.yaml", remoteCRDBody)
	url := cs.URL + "/manifests/main/crd.yaml"
	repo := t.TempDir()
	overlay := filepath.Join(repo, "crds")
	writeRemoteKustomization(t, overlay, url)
	cacheDir := t.TempDir()

	b := NewBuilder(repo, WithRemoteCache(cacheDir, time.Hour, 30*time.Second))
	if _, err := b.Build(context.Background(), overlay); err != nil {
		t.Fatalf("cold build: %v", err)
	}

	rc := newRemoteCache(cacheDir, time.Hour, 30*time.Second)
	path := rc.cachePath(url)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	b2 := NewBuilder(repo, WithRemoteCache(cacheDir, time.Hour, 30*time.Second))
	if _, err := b2.Build(context.Background(), overlay); err != nil {
		t.Fatalf("expired build: %v", err)
	}
	if n := cs.getsCount(); n != 2 {
		t.Fatalf("expired: %d GETs, want 2 (expired entry re-fetched)", n)
	}
}

// TestRemoteCache_FloatingUnreachableUsesStale: an expired entry whose refresh
// fails falls back to the stale copy with a warning instead of failing.
func TestRemoteCache_FloatingUnreachableUsesStale(t *testing.T) {
	cs := newCountingServer(t, "/manifests/main/crd.yaml", remoteCRDBody)
	url := cs.URL + "/manifests/main/crd.yaml"
	repo := t.TempDir()
	overlay := filepath.Join(repo, "crds")
	writeRemoteKustomization(t, overlay, url)
	cacheDir := t.TempDir()

	b := NewBuilder(repo, WithRemoteCache(cacheDir, time.Hour, 30*time.Second))
	warmOut, err := b.Build(context.Background(), overlay)
	if err != nil {
		t.Fatalf("warm-up build: %v", err)
	}

	// Expire the entry, then kill the server.
	rc := newRemoteCache(cacheDir, time.Hour, 30*time.Second)
	path := rc.cachePath(url)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	cs.close()

	var staleOut []byte
	stderr := captureStderr(t, func() {
		b2 := NewBuilder(repo, WithRemoteCache(cacheDir, time.Hour, 30*time.Second))
		var err error
		staleOut, err = b2.Build(context.Background(), overlay)
		if err != nil {
			t.Errorf("stale-fallback build failed: %v", err)
		}
	})
	if !bytes.Equal(warmOut, staleOut) {
		t.Fatalf("stale-fallback output differs from warm output")
	}
	if !strings.Contains(stderr, "using cached copy") {
		t.Fatalf("expected stale-fallback warning on stderr, got: %q", stderr)
	}
}

// TestRemoteCache_CorruptCacheFileRedownloads: a corrupted cache entry is
// detected and re-downloaded rather than served to kustomize.
func TestRemoteCache_CorruptCacheFileRedownloads(t *testing.T) {
	cs := newCountingServer(t, "/manifests/main/crd.yaml", remoteCRDBody)
	url := cs.URL + "/manifests/main/crd.yaml"
	repo := t.TempDir()
	overlay := filepath.Join(repo, "crds")
	writeRemoteKustomization(t, overlay, url)
	cacheDir := t.TempDir()

	b := NewBuilder(repo, WithRemoteCache(cacheDir, time.Hour, 30*time.Second))
	if _, err := b.Build(context.Background(), overlay); err != nil {
		t.Fatalf("cold build: %v", err)
	}

	rc := newRemoteCache(cacheDir, time.Hour, 30*time.Second)
	if err := os.WriteFile(rc.cachePath(url), []byte("{{{ not yaml"), 0o644); err != nil {
		t.Fatalf("corrupting cache file: %v", err)
	}

	b2 := NewBuilder(repo, WithRemoteCache(cacheDir, time.Hour, 30*time.Second))
	out, err := b2.Build(context.Background(), overlay)
	if err != nil {
		t.Fatalf("build over corrupt cache: %v", err)
	}
	if !strings.Contains(string(out), "from-remote") {
		t.Fatalf("output missing remote resource after repair:\n%s", out)
	}
	if n := cs.getsCount(); n != 2 {
		t.Fatalf("corrupt cache: %d GETs, want 2 (re-download)", n)
	}
}

// TestRemoteCache_UnresolvableURLFallsBack: when a URL cannot be downloaded
// and has no cached copy, it is left for kustomize — the build then fails
// exactly as it did before the cache existed (strict semantics preserved).
func TestRemoteCache_UnresolvableURLFallsBack(t *testing.T) {
	cs := newCountingServer(t, "/manifests/main/crd.yaml", remoteCRDBody)
	url := cs.URL + "/manifests/main/crd.yaml"
	cs.close() // dead server, empty cache: nothing to serve

	repo := t.TempDir()
	overlay := filepath.Join(repo, "crds")
	writeRemoteKustomization(t, overlay, url)

	var err error
	stderr := captureStderr(t, func() {
		b := NewBuilder(repo, WithRemoteCache(t.TempDir(), time.Hour, 30*time.Second))
		_, err = b.Build(context.Background(), overlay)
	})
	if err == nil {
		t.Fatal("expected build error when remote is unreachable and cache is empty")
	}
	if !strings.Contains(stderr, "could not download remote resource") {
		t.Fatalf("expected download-failure warning on stderr, got: %q", stderr)
	}
}

// TestRemoteCache_RelativeBasesStillWork: with the cache enabled, intra-repo
// relative bases (Flux's ../../base pattern) keep working alongside rewritten
// remote URLs — builds still run against real repository paths.
func TestRemoteCache_RelativeBasesStillWork(t *testing.T) {
	cs := newCountingServer(t, "/manifests/main/crd.yaml", remoteCRDBody)
	url := cs.URL + "/manifests/main/crd.yaml"
	repo := t.TempDir()

	base := filepath.Join(repo, "apps", "base")
	writeTestFile(t, filepath.Join(base, "cm.yaml"),
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: shared\n")
	writeTestFile(t, filepath.Join(base, "kustomization.yaml"),
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - cm.yaml\n")

	overlay := filepath.Join(repo, "clusters", "test", "app")
	writeTestFile(t, filepath.Join(overlay, "kustomization.yaml"), fmt.Sprintf(
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - ../../../apps/base\n  - %s\n", url))

	b := NewBuilder(repo, WithRemoteCache(t.TempDir(), time.Hour, 30*time.Second))
	out, err := b.Build(context.Background(), overlay)
	if err != nil {
		t.Fatalf("build with relative base + remote URL: %v", err)
	}
	if !strings.Contains(string(out), "name: shared") || !strings.Contains(string(out), "name: from-remote") {
		t.Fatalf("output missing base or remote resource:\n%s", out)
	}
}

// TestRemoteCache_ScanSkipsGitDir: kustomization files inside .git are never
// scanned or rewritten.
func TestRemoteCache_ScanSkipsGitDir(t *testing.T) {
	cs := newCountingServer(t, "/manifests/main/crd.yaml", remoteCRDBody)
	url := cs.URL + "/manifests/main/crd.yaml"
	repo := t.TempDir()
	writeRemoteKustomization(t, filepath.Join(repo, ".git"), url)

	fileRefs, dirRefs := scanRemoteRefs(repo)
	if len(fileRefs) != 0 || len(dirRefs) != 0 {
		t.Fatalf("scan picked up refs from .git: files=%v dirs=%v", fileRefs, dirRefs)
	}
}

// TestRemoteCache_ComponentsNotCachedWarns: components must resolve to
// directories (kustomize clones them); the cache only warns and leaves them
// untouched.
func TestRemoteCache_ComponentsNotCachedWarns(t *testing.T) {
	repo := t.TempDir()
	writeTestFile(t, filepath.Join(repo, "overlay", "kustomization.yaml"),
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: []\ncomponents:\n  - https://github.com/org/repo/components?ref=main\n")

	fileRefs, dirRefs := scanRemoteRefs(repo)
	if len(fileRefs) != 0 {
		t.Fatalf("components must not be classified as file refs: %v", fileRefs)
	}
	if len(dirRefs) != 1 {
		t.Fatalf("expected 1 dir ref, got %v", dirRefs)
	}

	rc := newRemoteCache(t.TempDir(), time.Hour, 30*time.Second)
	stderr := captureStderr(t, func() {
		rc.prepare(context.Background(), repo)
	})
	if !strings.Contains(stderr, "remote component") {
		t.Fatalf("expected unsupported-component warning, got: %q", stderr)
	}
	if _, ok := rc.resolve("https://github.com/org/repo/components?ref=main"); ok {
		t.Fatal("component URL must not be resolved to a cache path")
	}
}

// TestRestrictedFs_ExtraRootIsNarrow: with the remote cache enabled, reads are
// allowed inside the cache directory but nowhere else outside the repository —
// the cache root does not open up its parent directory.
func TestRestrictedFs_ExtraRootIsNarrow(t *testing.T) {
	base := t.TempDir()
	cacheDir := filepath.Join(base, "kustomize-remote")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeTestFile(t, filepath.Join(cacheDir, "entry.yaml"), "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cached\n")
	outside := filepath.Join(base, "secret.yaml") // cache dir's parent
	writeTestFile(t, outside, "apiVersion: v1\nkind: Secret\nmetadata:\n  name: leaked\n")

	repo := t.TempDir()
	fs := newRestrictedFs(repo, cacheDir)

	if _, err := fs.ReadFile(filepath.Join(cacheDir, "entry.yaml")); err != nil {
		t.Fatalf("reading inside cache dir should be allowed: %v", err)
	}
	if _, err := fs.ReadFile(outside); err == nil {
		t.Fatal("reading outside repo and cache dir must be blocked")
	}
}

// TestRemoteCache_RewriteKeepsUnresolvedURLs: URLs the cache could not resolve
// stay in the rewritten content untouched, so kustomize still sees them.
func TestRemoteCache_RewriteKeepsUnresolvedURLs(t *testing.T) {
	rc := newRemoteCache(t.TempDir(), time.Hour, 30*time.Second)
	inner := newRestrictedFs(t.TempDir())
	fs := rc.wrapFs(inner).(*rewritingFs)

	dir := t.TempDir()
	kustPath := filepath.Join(dir, "kustomization.yaml")
	original := `# top comment
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - https://raw.githubusercontent.com/org/repo/main/crd.yaml # keep me
  - local.yaml
`
	writeTestFile(t, kustPath, original)

	data, changed := fs.rewriteIfKustomization(kustPath, []byte(original))
	if changed {
		t.Fatal("content with only unresolved URLs must stay unchanged")
	}
	if string(data) != original {
		t.Fatalf("unchanged content was modified:\n%s", data)
	}
}

// TestRemoteCache_RewritePreservesShape verifies the YAML round-trip of the
// rewrite: comments and key order survive, only resolved URL scalars change.
func TestRemoteCache_RewritePreservesShape(t *testing.T) {
	rc := newRemoteCache(t.TempDir(), time.Hour, 30*time.Second)
	rc.remember("https://raw.githubusercontent.com/org/repo/main/crd.yaml", "/cache/abc.yaml")

	dir := t.TempDir()
	kustPath := filepath.Join(dir, "kustomization.yaml")
	original := `# cluster overlay
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - https://raw.githubusercontent.com/org/repo/main/crd.yaml # pinned comment
  - local.yaml
`
	writeTestFile(t, kustPath, original)

	fs := rc.wrapFs(newRestrictedFs(dir)).(*rewritingFs)
	data, err := fs.ReadFile(kustPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		"# cluster overlay",
		"# pinned comment",
		"- /cache/abc.yaml",
		"- local.yaml",
		"kind: Kustomization",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rewritten kustomization missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "raw.githubusercontent.com") {
		t.Fatalf("resolved URL not substituted:\n%s", got)
	}

	// And the build-side read path (Open) serves the same bytes.
	f, err := fs.Open(kustPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(f); err != nil {
		t.Fatalf("reading via Open: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("Open serves different bytes than ReadFile")
	}
}

// TestIsPinnedURL covers the pinned/floating classification.
func TestIsPinnedURL(t *testing.T) {
	cases := []struct {
		url    string
		pinned bool
	}{
		// raw.githubusercontent.com/<owner>/<repo>/<ref>/…
		{"https://raw.githubusercontent.com/external-secrets/external-secrets/v2.10.0/deploy/crds/bundle.yaml", true},
		{"https://raw.githubusercontent.com/org/repo/main/deploy/crd.yaml", false},
		{"https://raw.githubusercontent.com/org/repo/master/deploy/crd.yaml", false},
		{"https://raw.githubusercontent.com/org/repo/HEAD/deploy/crd.yaml", false},
		{"https://raw.githubusercontent.com/org/repo/8ff1b6f4dd8dd8f0e2ff1b6da0d0f8fd/deploy/crd.yaml", true}, // commit sha
		{"https://raw.githubusercontent.com/org/repo/1.2.3/deploy/crd.yaml", true},
		{"https://raw.githubusercontent.com/org/repo/v2/deploy/crd.yaml", false}, // single number: likely a branch
		{"https://raw.githubusercontent.com/org/repo/20250101/crd.yaml", false},  // date-like branch name
		{"https://raw.githubusercontent.com/org/repo/1.2/deploy/crd.yaml", true}, // x.y is enough
		// github.com release assets
		{"https://github.com/jetstack/cert-manager/releases/download/v1.21.1/cert-manager.crds.yaml", true},
		{"https://github.com/jetstack/cert-manager/releases/download/1.21.1/cert-manager.crds.yaml", true},
		{"https://github.com/org/repo/releases/download/latest/crd.yaml", false},
		{"https://github.com/org/repo/releases/download/20250101/crd.yaml", false}, // date-like tag: not a sha (no a-f), not x.y
		// ?ref=
		{"https://example.com/manifests?ref=v1.0.0", true},
		{"https://example.com/manifests?ref=1.0.0", true},
		{"https://example.com/manifests?ref=1.2", true},
		{"https://example.com/manifests?ref=v2", false},
		{"https://example.com/manifests?ref=main", false},
		{"https://example.com/manifests?ref=8ff1b6f", true},  // short sha
		{"https://example.com/manifests?ref=1234567", false}, // all digits: not treated as sha
		// repo/directory bases (phase 2) and everything else: floating
		{"https://github.com/org/repo/manifests?ref=main", false},
		{"https://example.com/manifests/main/crd.yaml", false},
		{"http://example.com/crd.yaml", false},
	}
	for _, tc := range cases {
		if got := isPinnedURL(tc.url); got != tc.pinned {
			t.Errorf("isPinnedURL(%q) = %v, want %v", tc.url, got, tc.pinned)
		}
	}
}

// TestRestrictedFs_ExtraRootIsReadOnly: the cache directory is admitted for
// reads only — builds must never create, write or delete anything in it (the
// cache is written exclusively by the downloader).
func TestRestrictedFs_ExtraRootIsReadOnly(t *testing.T) {
	cacheDir := t.TempDir()
	repo := t.TempDir()
	fs := newRestrictedFs(repo, cacheDir)

	target := filepath.Join(cacheDir, "evil.yaml")
	if err := fs.WriteFile(target, []byte("x")); err == nil {
		t.Error("WriteFile into cache dir must be rejected")
	}
	if _, err := fs.Create(target); err == nil {
		t.Error("Create in cache dir must be rejected")
	}
	if err := fs.MkdirAll(filepath.Join(cacheDir, "sub")); err == nil {
		t.Error("MkdirAll in cache dir must be rejected")
	}
	if err := fs.RemoveAll(cacheDir); err == nil {
		t.Error("RemoveAll on cache dir must be rejected")
	}
	// Reads still work.
	writeTestFile(t, target, "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: ok\n")
	if _, err := fs.ReadFile(target); err != nil {
		t.Fatalf("reading inside cache dir should be allowed: %v", err)
	}
}

// TestRemoteCache_HTTP404FallsBack: a non-200 response leaves the URL
// uncached with a warning, for kustomize to handle as it did before.
func TestRemoteCache_HTTP404FallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	rc := newRemoteCache(t.TempDir(), time.Hour, 30*time.Second)
	var path string
	var ok bool
	stderr := captureStderr(t, func() {
		path, ok = rc.ensure(context.Background(), srv.URL+"/missing.yaml")
	})
	if ok || path != "" {
		t.Fatalf("404 must not resolve to a cache path, got (%q, %v)", path, ok)
	}
	if !strings.Contains(stderr, "could not download remote resource") {
		t.Fatalf("expected download-failure warning, got: %q", stderr)
	}
	if !strings.Contains(stderr, "404") {
		t.Fatalf("warning should mention the HTTP status, got: %q", stderr)
	}
	// No cache file was written.
	if entries, _ := os.ReadDir(rc.dir); len(entries) > 0 {
		t.Fatalf("404 must not leave files in the cache: %v", entries)
	}
}

// TestKustomizeRejectsKindlessResource documents the assumption behind
// isManifestYAML requiring apiVersion+kind: kustomize itself refuses resource
// files without k8s object metadata, so the cache admits only that shape and
// loses no coverage. If a future kustomize release starts accepting kindless
// resources, this canary fails and the validation needs revisiting.
func TestKustomizeRejectsKindlessResource(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "plain.yaml"), "foo: bar\n")
	writeTestFile(t, filepath.Join(dir, "kustomization.yaml"),
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - plain.yaml\n")
	if _, err := NewBuilder(dir).Build(context.Background(), dir); err == nil {
		t.Fatal("kustomize accepted a kindless resource — isManifestYAML's apiVersion+kind requirement now under-caches; revisit the validation")
	}
}

// TestIsManifestYAML covers the cache validation shape directly.
func TestIsManifestYAML(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{"manifest", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n", true},
		{"multi-doc bundle", "apiVersion: v1\nkind: CustomResourceDefinition\nmetadata:\n  name: x\n---\napiVersion: v1\nkind: ConfigMap\n", true},
		{"scalar (one-line html)", "<html><body>page</body></html>", false},
		{"sequence", "- apiVersion: v1\n  kind: ConfigMap\n", false},
		{"mapping without kind (tag garbage)", "title: Not a resource\ndescription: x\n", false},
		{"mapping with kind but no apiVersion", "kind: ConfigMap\n", false},
		{"empty", "", false},
		{"garbage", "{{{ not yaml", false},
	}
	for _, tc := range cases {
		if got := isManifestYAML([]byte(tc.data)); got != tc.want {
			t.Errorf("isManifestYAML(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestRemoteCache_NonYAMLBodyFallsBack: a 200 response whose body is not a
// manifest (an HTML page from a repo/directory base) is not cached and is left
// for kustomize's own git-based resolution.
func TestRemoteCache_NonYAMLBodyFallsBack(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body>repository page</body></html>")
	}))
	t.Cleanup(srv.Close)

	rc := newRemoteCache(t.TempDir(), time.Hour, 30*time.Second)
	var ok bool
	stderr := captureStderr(t, func() {
		_, ok = rc.ensure(context.Background(), srv.URL+"/org/repo")
	})
	if ok {
		t.Fatal("non-YAML body must not resolve to a cache path")
	}
	if !strings.Contains(stderr, "not a YAML manifest") {
		t.Fatalf("expected not-a-manifest warning, got: %q", stderr)
	}
	if entries, _ := os.ReadDir(rc.dir); len(entries) > 0 {
		t.Fatalf("non-YAML body must not leave files in the cache: %v", entries)
	}
}

// TestRemoteCache_RelativeCacheDirAnchoredToCWD: a relative cache directory
// (CI passes --remote-cache-dir .cache/...) must resolve against the
// process working directory — never against a kustomization's own directory.
// Regression test: with a relative dir the rewritten resource path stayed
// relative, kustomize resolved it against the nested kustomization root and
// the build failed with "no such file or directory" (CI: .cache-fluxview
// looked up under k8s/crds/cert-manager/).
func TestRemoteCache_RelativeCacheDirAnchoredToCWD(t *testing.T) {
	cs := newCountingServer(t, "/crds/bundle.yaml", remoteCRDBody)
	url := cs.URL + "/crds/bundle.yaml"

	workDir := t.TempDir() // process CWD for the test
	repo := t.TempDir()    // repo root, deliberately different from CWD
	overlay := filepath.Join(repo, "k8s", "clusters", "bss", "test", "crds")
	writeRemoteKustomization(t, overlay, url)

	t.Chdir(workDir)

	// Relative cache dir: expected to land at <CWD>/rel-cache, and the
	// rewritten path inside the kustomization to be absolute.
	b := NewBuilder(repo, WithRemoteCache("rel-cache", time.Hour, 30*time.Second))
	out, err := b.Build(context.Background(), overlay)
	if err != nil {
		t.Fatalf("build with relative cache dir: %v", err)
	}
	if !strings.Contains(string(out), "from-remote") {
		t.Fatalf("output missing remote resource:\n%s", out)
	}

	rc := newRemoteCache("rel-cache", time.Hour, 30*time.Second)
	if !filepath.IsAbs(rc.dir) {
		t.Fatalf("cache dir must be absolutized, got %q", rc.dir)
	}
	if want := filepath.Join(workDir, "rel-cache"); rc.dir != want {
		t.Fatalf("cache dir = %q, want %q (anchored to CWD)", rc.dir, want)
	}
}

// TestRemoteCacheDisabledViaDir: WithRemoteCache with the shared disable
// spell leaves the Builder without a remote cache.
func TestRemoteCacheDisabledViaDir(t *testing.T) {
	for _, dir := range []string{"off", "none", "DISABLED"} {
		if b := NewBuilder(t.TempDir(), WithRemoteCache(dir, time.Hour, 30*time.Second)); b.remote != nil {
			t.Fatalf("remote cache must be disabled with dir=%q", dir)
		}
	}
	if b := NewBuilder(t.TempDir(), WithRemoteCache("", time.Hour, 30*time.Second)); b.remote == nil {
		t.Fatal("empty dir must keep the remote cache enabled (default dir)")
	}
}

// TestDefaultRemoteCacheTTLAndDir covers the defaults and the
// newRemoteCache normalization of empty dir / negative TTL.
func TestDefaultRemoteCacheTTLAndDir(t *testing.T) {
	if got := DefaultRemoteCacheTTL(); got != 10*time.Minute {
		t.Errorf("DefaultRemoteCacheTTL() = %s, want 10m", got)
	}
	if got := DefaultRemoteCacheTimeout(); got != 30*time.Second {
		t.Errorf("DefaultRemoteCacheTimeout() = %s, want 30s", got)
	}
	t.Setenv("FLUXVIEW_REMOTE_CACHE_TIMEOUT", "5m")
	if got := DefaultRemoteCacheTimeout(); got != 5*time.Minute {
		t.Errorf("DefaultRemoteCacheTimeout() = %s, want 5m (env)", got)
	}
	warnEnvTimeoutOnce = sync.Once{}
	captureStderr(t, func() {
		t.Setenv("FLUXVIEW_REMOTE_CACHE_TIMEOUT", "bogus")
		if got := DefaultRemoteCacheTimeout(); got != 30*time.Second {
			t.Errorf("DefaultRemoteCacheTimeout() = %s, want 30s fallback on invalid env", got)
		}
	})
	t.Setenv("FLUXVIEW_REMOTE_CACHE_TIMEOUT", "0")
	if got := DefaultRemoteCacheTimeout(); got != 0 {
		t.Errorf("DefaultRemoteCacheTimeout() = %s, want 0 (env no-limit)", got)
	}

	t.Setenv("XDG_CACHE_HOME", "/custom-xdg")
	if got := DefaultRemoteCacheDir(); got != "/custom-xdg/fluxview/kustomize-remote" {
		t.Errorf("DefaultRemoteCacheDir() = %s, want /custom-xdg/fluxview/kustomize-remote", got)
	}

	// An explicitly empty cache dir (e.g. --remote-cache-dir=) means
	// "default", and negative TTL/timeout values are normalized to 0 with
	// one explanatory warning each.
	var rc *remoteCache
	stderr := captureStderr(t, func() {
		rc = newRemoteCache("", -time.Minute, -time.Minute)
	})
	if !strings.Contains(stderr, "negative kustomize remote cache TTL -1m0s, treating as 0 (always refresh)") {
		t.Errorf("expected negative-TTL warning, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "negative kustomize remote download timeout -1m0s, treating as 0 (no limit)") {
		t.Errorf("expected negative-timeout warning, got:\n%s", stderr)
	}
	if rc.dir != "/custom-xdg/fluxview/kustomize-remote" {
		t.Errorf("newRemoteCache dir = %s, want default %s", rc.dir, "/custom-xdg/fluxview/kustomize-remote")
	}
	if rc.ttl != 0 {
		t.Errorf("newRemoteCache ttl = %s, want 0 (negative normalized)", rc.ttl)
	}
	if rc.client.Timeout != 0 {
		t.Errorf("newRemoteCache client timeout = %s, want 0 (negative normalized to no limit)", rc.client.Timeout)
	}
}

// TestRemoteCache_RepeatedBuildsShareResolution: within one Builder the same
// URL is downloaded once even if many directories reference it.
func TestRemoteCache_RepeatedBuildsShareResolution(t *testing.T) {
	cs := newCountingServer(t, "/manifests/main/crd.yaml", remoteCRDBody)
	url := cs.URL + "/manifests/main/crd.yaml"
	repo := t.TempDir()
	writeRemoteKustomization(t, filepath.Join(repo, "a"), url)
	writeRemoteKustomization(t, filepath.Join(repo, "b"), url)

	b := NewBuilder(repo, WithRemoteCache(t.TempDir(), time.Hour, 30*time.Second))
	for _, dir := range []string{filepath.Join(repo, "a"), filepath.Join(repo, "b")} {
		if _, err := b.Build(context.Background(), dir); err != nil {
			t.Fatalf("build %s: %v", dir, err)
		}
	}
	if n := cs.getsCount(); n != 1 {
		t.Fatalf("%d GETs for two directories sharing one URL, want 1", n)
	}
}

// TestRemoteCache_DownloadTimeout: a remote slower than the configured
// per-request timeout fails fast, while timeout 0 (no limit) waits the
// download out — the slow-network escape hatch.
func TestRemoteCache_DownloadTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		fmt.Fprint(w, remoteCRDBody)
	}))
	t.Cleanup(slow.Close)
	url := slow.URL + "/crd.yaml"

	rc := newRemoteCache(t.TempDir(), time.Hour, 20*time.Millisecond)
	if _, ok := rc.ensure(context.Background(), url); ok {
		t.Fatal("ensure succeeded despite a timeout shorter than the server delay")
	}

	rc = newRemoteCache(t.TempDir(), time.Hour, 0)
	path, ok := rc.ensure(context.Background(), url)
	if !ok {
		t.Fatal("ensure with no timeout failed on a slow remote")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cached file missing: %v", err)
	}
}

// TestDefaultRemoteCacheTimeoutInvalidEnvWarnsOnce: an unparsable
// FLUXVIEW_REMOTE_CACHE_TIMEOUT falls back to the default and warns exactly
// once per process (same contract as the Helm env vars). The package Once is
// reset first so the test stays order-independent and re-runnable under
// -count=2+.
func TestDefaultRemoteCacheTimeoutInvalidEnvWarnsOnce(t *testing.T) {
	t.Setenv("FLUXVIEW_REMOTE_CACHE_TIMEOUT", "bogus")
	warnEnvTimeoutOnce = sync.Once{}

	first := captureStderr(t, func() {
		if d := DefaultRemoteCacheTimeout(); d != 30*time.Second {
			t.Errorf("DefaultRemoteCacheTimeout() = %s, want default 30s for invalid env", d)
		}
	})
	if !strings.Contains(first, `invalid FLUXVIEW_REMOTE_CACHE_TIMEOUT "bogus"`) {
		t.Errorf("expected invalid-env warning, got:\n%s", first)
	}

	second := captureStderr(t, func() {
		if d := DefaultRemoteCacheTimeout(); d != 30*time.Second {
			t.Errorf("DefaultRemoteCacheTimeout() = %s, want default 30s for invalid env", d)
		}
	})
	if second != "" {
		t.Errorf("warning must not repeat, got:\n%s", second)
	}
}
