package helm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/helmpath"

	"github.com/banschikovde/fluxview/internal/flux"
)

// httpRepoServer serves a minimal Helm repository: index.yaml plus a single
// testchart-1.0.0.tgz, counting requests so tests can assert cache hits.
type httpRepoServer struct {
	*httptest.Server
	indexHits *atomic.Int32
	tgzHits   *atomic.Int32
}

func newHTTPRepoServer(t *testing.T) *httpRepoServer {
	return newHTTPRepoServerWithDelays(t, 0, 0)
}

// newHTTPRepoServerWithDelay serves the same minimal repository, stalling
// every response for delay — for download-timeout tests.
func newHTTPRepoServerWithDelay(t *testing.T, delay time.Duration) *httpRepoServer {
	return newHTTPRepoServerWithDelays(t, delay, delay)
}

// newHTTPRepoServerWithDelays stalls index.yaml and tarball responses
// independently, so a test can hold one fast and the other slow.
func newHTTPRepoServerWithDelays(t *testing.T, indexDelay, tgzDelay time.Duration) *httpRepoServer {
	t.Helper()

	// Build the chart archive once; its sha256 becomes the index digest, which
	// is the content-cache key.
	tgzPath := filepath.Join(t.TempDir(), "testchart-1.0.0.tgz")
	writeTgzChart(t, tgzPath, map[string]string{
		"Chart.yaml":        "apiVersion: v2\nname: testchart\nversion: 1.0.0\n",
		"values.yaml":       "replicas: \"1\"\n",
		"templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: snap\ndata:\n  replicas: {{ .Values.replicas | quote }}\n",
	})
	tgzData, err := os.ReadFile(tgzPath)
	if err != nil {
		t.Fatalf("read tgz: %v", err)
	}
	digest := sha256.Sum256(tgzData)

	var indexHits, tgzHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/index.yaml", func(w http.ResponseWriter, r *http.Request) {
		indexHits.Add(1)
		time.Sleep(indexDelay)
		fmt.Fprintf(w, `apiVersion: v1
entries:
  testchart:
    - apiVersion: v2
      created: "2026-01-01T00:00:00Z"
      description: test chart
      digest: sha256:%s
      name: testchart
      urls:
        - %s/testchart-1.0.0.tgz
      version: 1.0.0
generated: "2026-01-01T00:00:00Z"
`, hex.EncodeToString(digest[:]), "http://"+r.Host)
	})
	mux.HandleFunc("/testchart-1.0.0.tgz", func(w http.ResponseWriter, r *http.Request) {
		tgzHits.Add(1)
		time.Sleep(tgzDelay)
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(tgzData)
	})

	srv := &httpRepoServer{httptest.NewServer(mux), &indexHits, &tgzHits}
	t.Cleanup(srv.Close)
	return srv
}

func httpRepoHR() flux.HelmRelease {
	return flux.HelmRelease{
		Metadata: flux.ObjectMeta{Name: "app", Namespace: "test"},
		Spec: flux.HelmReleaseSpec{
			Chart: flux.HelmReleaseChart{Spec: flux.HelmReleaseChartSpec{
				Chart:   "testchart",
				Version: "1.0.0",
			}},
		},
	}
}

func inflateHTTP(t *testing.T, cacheDir string, ttl time.Duration, repoURL string) string {
	t.Helper()
	inflater, err := NewInflater(WithCacheDir(cacheDir), WithIndexTTL(ttl))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	out, err := inflater.InflateHelmRelease(context.Background(), httpRepoHR(), repoURL, "", "", nil, nil, "")
	if err != nil {
		t.Fatalf("InflateHelmRelease: %v", err)
	}
	return string(out)
}

// TestInflateHelmRelease_HTTPRepoCachedAcrossRuns: a second run over the same
// cache dir must not re-fetch the repo index (TTL) nor the chart tarball
// (content cache keyed by the index digest) — previously every run downloaded
// both, and diff hr downloaded them twice per invocation.
func TestInflateHelmRelease_HTTPRepoCachedAcrossRuns(t *testing.T) {
	srv := newHTTPRepoServer(t)
	cacheDir := t.TempDir()

	first := inflateHTTP(t, cacheDir, 10*time.Minute, srv.URL)
	if got := srv.indexHits.Load(); got != 1 {
		t.Errorf("first run: index fetches = %d, want 1", got)
	}
	if got := srv.tgzHits.Load(); got != 1 {
		t.Errorf("first run: chart downloads = %d, want 1", got)
	}

	second := inflateHTTP(t, cacheDir, 10*time.Minute, srv.URL)
	if got := srv.indexHits.Load(); got != 1 {
		t.Errorf("second run: index fetches = %d, want 1 (TTL cache)", got)
	}
	if got := srv.tgzHits.Load(); got != 1 {
		t.Errorf("second run: chart downloads = %d, want 1 (content cache)", got)
	}
	if first != second {
		t.Errorf("rendered output differs between runs:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

// TestInflateHelmRelease_HTTPIndexTTLZero: TTL=0 re-fetches the index on every
// run (fresh floating versions) while the pinned chart still comes from the
// content cache.
func TestInflateHelmRelease_HTTPIndexTTLZero(t *testing.T) {
	srv := newHTTPRepoServer(t)
	cacheDir := t.TempDir()

	inflateHTTP(t, cacheDir, 0, srv.URL)
	inflateHTTP(t, cacheDir, 0, srv.URL)

	if got := srv.indexHits.Load(); got != 2 {
		t.Errorf("index fetches = %d, want 2 (TTL=0 always refreshes)", got)
	}
	if got := srv.tgzHits.Load(); got != 1 {
		t.Errorf("chart downloads = %d, want 1 (content cache unaffected)", got)
	}
}

// cachedIndexPath mirrors how the Inflater lays out a repo's cached index,
// via the same SDK helper the production code uses.
func cachedIndexPath(cacheDir, repoURL string) string {
	return filepath.Join(cacheDir, "repository", helmpath.CacheIndexFile(repoCacheName(repoURL)))
}

// TestInflateHelmRelease_HTTPIndexTTLExpiry: an index older than the TTL is
// re-downloaded (simulated by back-dating the cached index file).
func TestInflateHelmRelease_HTTPIndexTTLExpiry(t *testing.T) {
	srv := newHTTPRepoServer(t)
	cacheDir := t.TempDir()

	inflateHTTP(t, cacheDir, time.Hour, srv.URL)

	// Back-date the cached index past the TTL.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(cachedIndexPath(cacheDir, srv.URL), old, old); err != nil {
		t.Fatalf("back-dating index: %v", err)
	}

	inflateHTTP(t, cacheDir, time.Hour, srv.URL)

	if got := srv.indexHits.Load(); got != 2 {
		t.Errorf("index fetches = %d, want 2 after TTL expiry", got)
	}
	if got := srv.tgzHits.Load(); got != 1 {
		t.Errorf("chart downloads = %d, want 1 (content cache unaffected)", got)
	}
}

// TestInflateHelmRelease_HTTPRepoOfflineWithCache: with a warm cache the tool
// keeps working when the repository is unreachable — the fresh-enough index is
// read from disk and the tarball from the content cache.
func TestInflateHelmRelease_HTTPRepoOfflineWithCache(t *testing.T) {
	srv := newHTTPRepoServer(t)
	cacheDir := t.TempDir()

	inflateHTTP(t, cacheDir, 10*time.Minute, srv.URL)
	srv.Close() // repository goes offline

	out := inflateHTTP(t, cacheDir, 10*time.Minute, srv.URL)
	if !strings.Contains(out, `kind: ConfigMap`) {
		t.Errorf("expected rendered ConfigMap from cached chart, got:\n%s", out)
	}
}

// TestInflateHelmRelease_HTTPRepoOfflineFallsBackToStaleIndex: TTL=0 forces an
// index refresh; when that fails but a stale index exists, it is used with a
// warning instead of failing the build.
func TestInflateHelmRelease_HTTPRepoOfflineFallsBackToStaleIndex(t *testing.T) {
	srv := newHTTPRepoServer(t)
	cacheDir := t.TempDir()

	inflateHTTP(t, cacheDir, 10*time.Minute, srv.URL)
	srv.Close() // repository goes offline

	stderr := captureStderrHelm(func() {
		inflateHTTP(t, cacheDir, 0, srv.URL)
	})
	if !strings.Contains(stderr, "using cached copy") {
		t.Errorf("expected stale-index warning on stderr, got:\n%s", stderr)
	}
}

// TestInflateHelmRelease_HTTPRepoDoesNotPersistCredentials: repository
// credentials (resolved from cluster Secrets) must never land in the on-disk
// repositories.yaml.
func TestInflateHelmRelease_HTTPRepoDoesNotPersistCredentials(t *testing.T) {
	srv := newHTTPRepoServer(t)
	cacheDir := t.TempDir()

	inflater, err := NewInflater(WithCacheDir(cacheDir))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	if _, err := inflater.InflateHelmRelease(context.Background(), httpRepoHR(), srv.URL, "user", "sekret-pass", nil, nil, ""); err != nil {
		t.Fatalf("InflateHelmRelease: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(cacheDir, "repositories.yaml"))
	if err != nil {
		t.Fatalf("reading repositories.yaml: %v", err)
	}
	if strings.Contains(string(data), "sekret-pass") {
		t.Errorf("repositories.yaml must not contain the password, got:\n%s", string(data))
	}
	if !strings.Contains(string(data), "username: \"\"") || !strings.Contains(string(data), "password: \"\"") {
		t.Errorf("repositories.yaml must have empty credential fields, got:\n%s", string(data))
	}
	if !strings.Contains(string(data), srv.URL) {
		t.Errorf("repositories.yaml should contain the repo URL, got:\n%s", string(data))
	}
}

// TestInflateHelmRelease_HTTPCorruptIndexRefreshed: a cached index that no
// longer parses is re-downloaded even within the TTL, instead of failing the
// build with a cryptic SDK error.
func TestInflateHelmRelease_HTTPCorruptIndexRefreshed(t *testing.T) {
	srv := newHTTPRepoServer(t)
	cacheDir := t.TempDir()

	inflateHTTP(t, cacheDir, time.Hour, srv.URL)

	if err := os.WriteFile(cachedIndexPath(cacheDir, srv.URL), []byte("not: [valid yaml"), 0644); err != nil {
		t.Fatalf("corrupting index: %v", err)
	}

	inflateHTTP(t, cacheDir, time.Hour, srv.URL)

	if got := srv.indexHits.Load(); got != 2 {
		t.Errorf("index fetches = %d, want 2 (corrupt cache re-downloaded)", got)
	}
	if got := srv.tgzHits.Load(); got != 1 {
		t.Errorf("chart downloads = %d, want 1 (content cache unaffected)", got)
	}
}

// TestInflateHelmRelease_SameInflaterRegistersRepoOnce: several HelmReleases
// inflated through one Inflater (one process) share a repository — its index
// must be validated/downloaded once, not per HelmRelease. TTL=0 makes the test
// falsifiable: without memoization every HelmRelease would force an index
// re-fetch (and the semantics it guards: TTL=0 refreshes once per process).
func TestInflateHelmRelease_SameInflaterRegistersRepoOnce(t *testing.T) {
	srv := newHTTPRepoServer(t)

	inflater, err := NewInflater(WithCacheDir(t.TempDir()), WithIndexTTL(0))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	for range 2 {
		if _, err := inflater.InflateHelmRelease(context.Background(), httpRepoHR(), srv.URL, "", "", nil, nil, ""); err != nil {
			t.Fatalf("InflateHelmRelease: %v", err)
		}
	}

	if got := srv.indexHits.Load(); got != 1 {
		t.Errorf("index fetches = %d, want 1 (registration memoized per repo)", got)
	}
	if got := srv.tgzHits.Load(); got != 1 {
		t.Errorf("chart downloads = %d, want 1 (content cache)", got)
	}
}

// TestNewInflaterNormalizesOptions: an explicitly empty cache dir falls back to
// the default (not CWD-relative paths) and a negative TTL behaves as 0.
func TestNewInflaterNormalizesOptions(t *testing.T) {
	defaultDir := t.TempDir()
	t.Setenv("FLUXVIEW_HELM_CACHE_DIR", defaultDir)

	var in *Inflater
	stderr := captureStderrHelm(func() {
		var err error
		in, err = NewInflater(WithCacheDir(""), WithIndexTTL(-time.Minute))
		if err != nil {
			t.Fatalf("NewInflater: %v", err)
		}
	})

	if want := filepath.Join(defaultDir, "repository"); in.settings.RepositoryCache != want {
		t.Errorf("RepositoryCache = %q, want default %q", in.settings.RepositoryCache, want)
	}
	if in.indexTTL != 0 {
		t.Errorf("indexTTL = %s, want 0 (negative normalized)", in.indexTTL)
	}
	if !strings.Contains(stderr, "negative Helm index TTL") {
		t.Errorf("expected negative-TTL warning on stderr, got:\n%s", stderr)
	}
}

// TestDefaultIndexTTLInvalidEnvWarnsOnce: an unparsable FLUXVIEW_HELM_INDEX_TTL
// falls back to the default and warns exactly once per process. The package
// Once is reset first so the test stays order-independent (another test may
// have consumed the warning already) and re-runnable under -count=2+.
func TestDefaultIndexTTLInvalidEnvWarnsOnce(t *testing.T) {
	t.Setenv("FLUXVIEW_HELM_INDEX_TTL", "bogus")
	warnEnvTTLOnce = sync.Once{}

	first := captureStderrHelm(func() {
		if ttl := DefaultIndexTTL(); ttl != 10*time.Minute {
			t.Errorf("DefaultIndexTTL() = %s, want default 10m for invalid env", ttl)
		}
	})
	if !strings.Contains(first, `invalid FLUXVIEW_HELM_INDEX_TTL "bogus"`) {
		t.Errorf("expected invalid-env warning, got:\n%s", first)
	}

	second := captureStderrHelm(func() {
		if ttl := DefaultIndexTTL(); ttl != 10*time.Minute {
			t.Errorf("DefaultIndexTTL() = %s, want default 10m for invalid env", ttl)
		}
	})
	if second != "" {
		t.Errorf("warning must not repeat, got:\n%s", second)
	}
}

// TestDefaultIndexTTL covers the env resolution paths: unset → default, valid
// duration honored, negative flows through (NewInflater normalizes it). The
// invalid-value path is covered separately by the warn-once test above.
func TestDefaultIndexTTL(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want time.Duration
	}{
		{name: "unset falls back to default", env: "", want: 10 * time.Minute},
		{name: "valid duration honored", env: "45s", want: 45 * time.Second},
		{name: "negative flows through for NewInflater to normalize", env: "-5m", want: -5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("FLUXVIEW_HELM_INDEX_TTL", tt.env)
			if got := DefaultIndexTTL(); got != tt.want {
				t.Errorf("DefaultIndexTTL() = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestRepoCacheName is stable and filesystem-safe.
func TestRepoCacheName(t *testing.T) {
	a, b := repoCacheName("https://charts.example.com"), repoCacheName("https://charts.example.com")
	if a != b {
		t.Errorf("repoCacheName not deterministic: %q vs %q", a, b)
	}
	if a != filepath.Base(a) || strings.ContainsAny(a, "/\\") {
		t.Errorf("repoCacheName %q is not a safe file name", a)
	}
}

// TestDefaultDownloadTimeoutInvalidEnvWarnsOnce: an unparsable
// FLUXVIEW_HELM_DOWNLOAD_TIMEOUT falls back to the default and warns exactly
// once per process (same contract as the index TTL env).
func TestDefaultDownloadTimeoutInvalidEnvWarnsOnce(t *testing.T) {
	t.Setenv("FLUXVIEW_HELM_DOWNLOAD_TIMEOUT", "bogus")
	warnEnvTimeoutOnce = sync.Once{}

	first := captureStderrHelm(func() {
		if d := DefaultDownloadTimeout(); d != 120*time.Second {
			t.Errorf("DefaultDownloadTimeout() = %s, want default 2m for invalid env", d)
		}
	})
	if !strings.Contains(first, `invalid FLUXVIEW_HELM_DOWNLOAD_TIMEOUT "bogus"`) {
		t.Errorf("expected invalid-env warning, got:\n%s", first)
	}

	second := captureStderrHelm(func() {
		if d := DefaultDownloadTimeout(); d != 120*time.Second {
			t.Errorf("DefaultDownloadTimeout() = %s, want default 2m for invalid env", d)
		}
	})
	if second != "" {
		t.Errorf("warning must not repeat, got:\n%s", second)
	}
}

// TestDefaultDownloadTimeout covers the env resolution paths: unset → the
// SDK-matching 120s default, valid durations honored (including 0 = no
// limit), negative flows through (NewInflater normalizes it).
func TestDefaultDownloadTimeout(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want time.Duration
	}{
		{name: "unset falls back to default", env: "", want: 120 * time.Second},
		{name: "valid duration honored", env: "30m", want: 30 * time.Minute},
		{name: "zero means no limit", env: "0", want: 0},
		{name: "negative flows through for NewInflater to normalize", env: "-5m", want: -5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("FLUXVIEW_HELM_DOWNLOAD_TIMEOUT", tt.env)
			if got := DefaultDownloadTimeout(); got != tt.want {
				t.Errorf("DefaultDownloadTimeout() = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestInflateHelmRelease_DownloadTimeout: a repository slower than the
// configured per-request timeout fails the inflation fast, while timeout 0
// (no limit) waits the slow downloads out — the slow-network escape hatch.
// The success leg also exercises the repo/chart download path end to end,
// which routes through the Inflater's own getters (not the SDK's
// LocateChart) precisely so this timeout applies.
func TestInflateHelmRelease_DownloadTimeout(t *testing.T) {
	srv := newHTTPRepoServerWithDelay(t, 300*time.Millisecond)

	inflater, err := NewInflater(WithCacheDir(t.TempDir()), WithDownloadTimeout(20*time.Millisecond))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	if _, err := inflater.InflateHelmRelease(context.Background(), httpRepoHR(), srv.URL, "", "", nil, nil, ""); err == nil {
		t.Fatal("inflation succeeded despite a download timeout shorter than the server delay")
	}

	inflater, err = NewInflater(WithCacheDir(t.TempDir()), WithDownloadTimeout(0))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	if _, err := inflater.InflateHelmRelease(context.Background(), httpRepoHR(), srv.URL, "", "", nil, nil, ""); err != nil {
		t.Fatalf("inflation with no download timeout failed on a slow repo: %v", err)
	}
}

// TestInflateHelmRelease_DownloadTimeoutTarball: the timeout must also bound
// the tarball leg of the repo/chart download (the index resolves instantly,
// the chart download stalls) — the path that routes through the Inflater's
// own ChartDownloader rather than the SDK's LocateChart.
func TestInflateHelmRelease_DownloadTimeoutTarball(t *testing.T) {
	srv := newHTTPRepoServerWithDelays(t, 0, 300*time.Millisecond)

	inflater, err := NewInflater(WithCacheDir(t.TempDir()), WithDownloadTimeout(20*time.Millisecond))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	_, err = inflater.InflateHelmRelease(context.Background(), httpRepoHR(), srv.URL, "", "", nil, nil, "")
	if err == nil {
		t.Fatal("inflation succeeded despite a tarball download slower than the timeout")
	}
	if !strings.Contains(err.Error(), "locating chart") {
		t.Errorf("expected a chart-locating failure, got: %v", err)
	}
	// The index resolved fine before the tarball aborted.
	if got := srv.indexHits.Load(); got != 1 {
		t.Errorf("index fetches = %d, want 1 (timeout applies to the tarball leg)", got)
	}
	if got := srv.tgzHits.Load(); got != 1 {
		t.Errorf("tarball requests = %d, want 1", got)
	}
}

// TestNewInflaterNegativeDownloadTimeoutWarns: a negative timeout is
// normalized to 0 (no limit) with a single explanatory warning.
func TestNewInflaterNegativeDownloadTimeoutWarns(t *testing.T) {
	stderr := captureStderrHelm(func() {
		in, err := NewInflater(WithCacheDir(t.TempDir()), WithDownloadTimeout(-time.Minute))
		if err != nil {
			t.Fatalf("NewInflater: %v", err)
		}
		if in.downloadTimeout != 0 {
			t.Errorf("downloadTimeout = %s, want 0 (negative normalized to no limit)", in.downloadTimeout)
		}
	})
	if !strings.Contains(stderr, "negative Helm download timeout -1m0s, treating as 0 (no limit)") {
		t.Errorf("expected negative-timeout warning, got:\n%s", stderr)
	}
}
