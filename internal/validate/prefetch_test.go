package validate

import (
	"context"
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
)

func TestKindSuffix(t *testing.T) {
	tests := []struct{ apiVersion, want string }{
		{"v1", "-v1"},
		{"apps/v1", "-apps-v1"},
		{"helm.toolkit.fluxcd.io/v2", "-helm-v2"},
		{"rbac.authorization.k8s.io/v1", "-rbac-v1"},
	}
	for _, tt := range tests {
		if got := KindSuffix(tt.apiVersion); got != tt.want {
			t.Errorf("KindSuffix(%q) = %q, want %q", tt.apiVersion, got, tt.want)
		}
	}
}

func TestSchemaFileName(t *testing.T) {
	tests := []struct {
		kind, apiVersion, want string
	}{
		{"Deployment", "apps/v1", "deployment-apps-v1.json"},
		{"ConfigMap", "v1", "configmap-v1.json"},
		{"HelmRelease", "helm.toolkit.fluxcd.io/v2", "helmrelease-helm-v2.json"},
	}
	for _, tt := range tests {
		if got := SchemaFileName(tt.kind, tt.apiVersion); got != tt.want {
			t.Errorf("SchemaFileName(%q, %q) = %q, want %q", tt.kind, tt.apiVersion, got, tt.want)
		}
	}
}

func TestResourceKinds(t *testing.T) {
	kinds := ResourceKinds([]byte(testWidgetResource + `---
apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: other-widget
  spec: dup
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: dep
---
not: a kubernetes resource
---
`))

	want := []ResourceKind{
		{APIVersion: "test.example.com/v1", Kind: "Widget"},
		{APIVersion: "apps/v1", Kind: "Deployment"},
	}
	if len(kinds) != len(want) {
		t.Fatalf("ResourceKinds = %+v, want %+v (deduped, malformed dropped)", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Errorf("kinds[%d] = %+v, want %+v", i, kinds[i], want[i])
		}
	}
}

func TestKindCoverage(t *testing.T) {
	flat := t.TempDir()
	writeFile(t, flat, "widget-test-v1.json", `{"type": "object"}`)

	versioned := t.TempDir()
	writeFile(t, versioned, "v1.36.1-standalone/configmap-v1.json", `{"type": "object"}`)
	writeFile(t, versioned, "v1.36.1-standalone-strict/deployment-apps-v1.json", `{"type": "object"}`)

	widget := ResourceKind{APIVersion: "test.example.com/v1", Kind: "Widget"}
	configMap := ResourceKind{APIVersion: "v1", Kind: "ConfigMap"}
	deployment := ResourceKind{APIVersion: "apps/v1", Kind: "Deployment"}

	t.Run("flat dirs cover any version", func(t *testing.T) {
		c := NewKindCoverage("1.30.0", false, []string{flat}, nil)
		if !c.Covers(widget) {
			t.Error("flat dir should cover the Widget schema")
		}
		if c.Covers(configMap) {
			t.Error("flat dir has no ConfigMap schema")
		}
	})

	t.Run("versioned roots cover only the requested variant", func(t *testing.T) {
		lenient := NewKindCoverage("1.36.1", false, nil, []string{versioned})
		if !lenient.Covers(configMap) {
			t.Error("checkout should cover ConfigMap in lenient mode")
		}
		if lenient.Covers(deployment) {
			t.Error("deployment schema exists only in the -strict variant, lenient mode must not claim it")
		}

		strict := NewKindCoverage("1.36.1", true, nil, []string{versioned})
		if strict.Covers(configMap) {
			t.Error("lenient configmap schema must not satisfy strict mode")
		}
		if !strict.Covers(deployment) {
			t.Error("strict checkout should cover Deployment")
		}

		other := NewKindCoverage("1.34.0", false, nil, []string{versioned})
		if other.Covers(configMap) {
			t.Error("a different kubernetes version must not be covered")
		}
	})
}

func TestUncoveredKinds(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "widget-test-v1.json", `{"type": "object"}`)
	coverage := NewKindCoverage("1.36.1", false, []string{dir}, nil)

	kinds := []ResourceKind{
		{APIVersion: "test.example.com/v1", Kind: "Widget"}, // covered locally
		{APIVersion: "v1", Kind: "ConfigMap"},               // needs the registry
		{APIVersion: "v1", Kind: "Secret"},                  // skipped by plain kind
		{APIVersion: "apps/v1", Kind: "Deployment"},         // skipped by apiVersion/Kind
		{APIVersion: "apps/v1", Kind: "StatefulSet"},        // needs the registry
	}

	got := UncoveredKinds(kinds, coverage, NormalizeSkipKinds([]string{"Secret", "apps/v1/Deployment"}))
	want := []ResourceKind{
		{APIVersion: "v1", Kind: "ConfigMap"},
		{APIVersion: "apps/v1", Kind: "StatefulSet"},
	}
	if len(got) != len(want) {
		t.Fatalf("UncoveredKinds = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("uncovered[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// registryStub serves the requested schema paths as {"type":"object"} JSON
// and counts requests per path.
type registryStub struct {
	url string
	mu  sync.Mutex
	// requests and total are guarded by mu: parallel prefetch workers hit
	// the handler concurrently.
	requests map[string]*int64
	total    int64
}

func newRegistryStub(t *testing.T, missing map[string]bool) *registryStub {
	t.Helper()
	stub := &registryStub{requests: map[string]*int64{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.total++
		counter, ok := stub.requests[r.URL.Path]
		if !ok {
			counter = new(int64)
			stub.requests[r.URL.Path] = counter
		}
		*counter++
		stub.mu.Unlock()
		if missing[r.URL.Path] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"type": "object"}`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	stub.url = server.URL
	return stub
}

func (s *registryStub) count(path string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.requests[path]; ok {
		return *c
	}
	return 0
}

func (s *registryStub) totalCount() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.total
}

func TestPrefetchDefaultSchemas(t *testing.T) {
	configMap := ResourceKind{APIVersion: "v1", Kind: "ConfigMap"}
	deployment := ResourceKind{APIVersion: "apps/v1", Kind: "Deployment"}

	t.Run("downloads into the checkout layout and caches across runs", func(t *testing.T) {
		stub := newRegistryStub(t, nil)
		cache := t.TempDir()

		missing, err := PrefetchDefaultSchemas(context.Background(), []ResourceKind{configMap, deployment}, PrefetchOptions{
			KubernetesVersion: "v1.36.1",
			CacheDir:          cache,
			BaseURL:           stub.url,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(missing) != 0 {
			t.Errorf("missing = %+v, want none", missing)
		}
		for _, name := range []string{"v1.36.1-standalone/configmap-v1.json", "v1.36.1-standalone/deployment-apps-v1.json"} {
			if _, err := os.Stat(filepath.Join(cache, name)); err != nil {
				t.Errorf("expected cached schema %s: %v", name, err)
			}
		}
		if got := stub.totalCount(); got != 2 {
			t.Errorf("registry requests = %d, want 2", got)
		}

		// Second run: everything is cached, no new requests.
		if _, err := PrefetchDefaultSchemas(context.Background(), []ResourceKind{configMap, deployment}, PrefetchOptions{
			KubernetesVersion: "1.36.1", CacheDir: cache, BaseURL: stub.url,
		}); err != nil {
			t.Fatal(err)
		}
		if got := stub.totalCount(); got != 2 {
			t.Errorf("cached run must not re-request, requests = %d, want 2", got)
		}
	})

	t.Run("strict selects the -strict variant", func(t *testing.T) {
		stub := newRegistryStub(t, nil)
		cache := t.TempDir()

		if _, err := PrefetchDefaultSchemas(context.Background(), []ResourceKind{configMap}, PrefetchOptions{
			KubernetesVersion: "1.36.1", Strict: true, CacheDir: cache, BaseURL: stub.url,
		}); err != nil {
			t.Fatal(err)
		}
		if got := stub.count("/v1.36.1-standalone-strict/configmap-v1.json"); got != 1 {
			t.Errorf("strict variant requests = %d, want 1", got)
		}
	})

	t.Run("404 is reported as missing, not an error", func(t *testing.T) {
		stub := newRegistryStub(t, map[string]bool{"/v1.36.1-standalone/configmap-v1.json": true})
		missing, err := PrefetchDefaultSchemas(context.Background(), []ResourceKind{configMap}, PrefetchOptions{
			KubernetesVersion: "1.36.1", CacheDir: t.TempDir(), BaseURL: stub.url,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(missing) != 1 || missing[0] != configMap {
			t.Errorf("missing = %+v, want [ConfigMap]", missing)
		}
	})

	t.Run("other HTTP failures fail closed after retries", func(t *testing.T) {
		var total int64
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt64(&total, 1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(server.Close)

		_, err := PrefetchDefaultSchemas(context.Background(), []ResourceKind{configMap}, PrefetchOptions{
			KubernetesVersion: "1.36.1", CacheDir: t.TempDir(), BaseURL: server.URL,
		})
		if err == nil {
			t.Fatal("a persistent 500 must fail the prefetch")
		}
		if got := atomic.LoadInt64(&total); got != 3 { // 1 attempt + 2 retries
			t.Errorf("attempts = %d, want 3", got)
		}
	})

	t.Run("hung requests are bounded by the timeout", func(t *testing.T) {
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-release
		}))
		t.Cleanup(func() {
			close(release)
			server.Close()
		})

		start := time.Now()
		_, err := PrefetchDefaultSchemas(context.Background(), []ResourceKind{configMap}, PrefetchOptions{
			KubernetesVersion: "1.36.1", CacheDir: t.TempDir(), BaseURL: server.URL,
			RequestTimeout: 50 * time.Millisecond,
		})
		if err == nil {
			t.Fatal("a hung registry must fail the prefetch, not hang forever")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Errorf("prefetch took %s, the request timeout should bound it", elapsed)
		}
	})

	t.Run("context cancellation aborts pending work", func(t *testing.T) {
		block := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-block
		}))
		t.Cleanup(func() { close(block); server.Close() })

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(100 * time.Millisecond)
			cancel()
		}()
		_, err := PrefetchDefaultSchemas(ctx, []ResourceKind{configMap}, PrefetchOptions{
			KubernetesVersion: "1.36.1", CacheDir: t.TempDir(), BaseURL: server.URL,
		})
		if err == nil {
			t.Fatal("cancellation must surface as an error")
		}
	})

	t.Run("non-JSON 200 responses fail and are not cached", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "<html>gateway error page</html>")
		}))
		t.Cleanup(server.Close)

		cache := t.TempDir()
		_, err := PrefetchDefaultSchemas(context.Background(), []ResourceKind{configMap}, PrefetchOptions{
			KubernetesVersion: "1.36.1", CacheDir: cache, BaseURL: server.URL,
		})
		if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
			t.Fatalf("a non-JSON 200 must fail the prefetch, got: %v", err)
		}
		entries, _ := os.ReadDir(filepath.Join(cache, "v1.36.1-standalone"))
		if len(entries) != 0 {
			t.Errorf("a garbage response must not be cached, got %d files", len(entries))
		}
	})

	t.Run("request timeout bounds a slow response; 0 and negative disable it differently", func(t *testing.T) {
		slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
			fmt.Fprint(w, `{"type": "object"}`)
		}))
		t.Cleanup(slow.Close)

		opts := func(timeout time.Duration) PrefetchOptions {
			return PrefetchOptions{
				KubernetesVersion: "1.36.1", CacheDir: t.TempDir(),
				BaseURL: slow.URL, Retries: 0, RequestTimeout: timeout,
			}
		}

		// Unset (0) picks the 30s default — a 300ms response fits.
		if _, err := PrefetchDefaultSchemas(context.Background(), []ResourceKind{configMap}, opts(0)); err != nil {
			t.Errorf("default timeout should allow a 300ms response, got: %v", err)
		}
		// Negative disables the per-request limit — also fits.
		o := opts(-1)
		if _, err := PrefetchDefaultSchemas(context.Background(), []ResourceKind{configMap}, o); err != nil {
			t.Errorf("no per-request timeout should allow a 300ms response, got: %v", err)
		}
		// An explicit small timeout cuts the request off.
		if _, err := PrefetchDefaultSchemas(context.Background(), []ResourceKind{configMap}, opts(50*time.Millisecond)); err == nil {
			t.Error("a 50ms timeout must fail a 300ms response")
		}
	})

	t.Run("no kinds to fetch is a no-op", func(t *testing.T) {
		if missing, err := PrefetchDefaultSchemas(context.Background(), nil, PrefetchOptions{
			KubernetesVersion: "1.36.1", CacheDir: t.TempDir(),
		}); err != nil || missing != nil {
			t.Errorf("PrefetchDefaultSchemas(nil) = (%v, %v), want (nil, nil)", missing, err)
		}
	})
}

func TestRegistryVersionDir(t *testing.T) {
	tests := []struct {
		version string
		strict  bool
		want    string
	}{
		{"1.36.1", false, "v1.36.1-standalone"},
		{"v1.36.1", true, "v1.36.1-standalone-strict"},
		{"master", false, "master-standalone"},
		// Strict applies to master too, mirroring kubeconform's template
		// rendering — pin it so an "obvious simplification" cannot diverge.
		{"master", true, "master-standalone-strict"},
		{"", false, "v" + DefaultKubernetesVersion + "-standalone"},
	}
	for _, tt := range tests {
		if got := RegistryVersionDir(tt.version, tt.strict); got != tt.want {
			t.Errorf("RegistryVersionDir(%q, %v) = %q, want %q", tt.version, tt.strict, got, tt.want)
		}
	}
}

func TestIsFluxKind(t *testing.T) {
	flux := []ResourceKind{
		{APIVersion: "kustomize.toolkit.fluxcd.io/v1", Kind: "Kustomization"},
		{APIVersion: "source.toolkit.fluxcd.io/v1", Kind: "GitRepository"},
		{APIVersion: "helm.toolkit.fluxcd.io/v2", Kind: "HelmRelease"},
		{APIVersion: "notification.toolkit.fluxcd.io/v1beta3", Kind: "Alert"},
	}
	for _, k := range flux {
		if !IsFluxKind(k) {
			t.Errorf("IsFluxKind(%s) = false, want true", k.GroupVersionKind())
		}
	}

	notFlux := []ResourceKind{
		{APIVersion: "v1", Kind: "ConfigMap"},
		{APIVersion: "apps/v1", Kind: "Deployment"},
		{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		{APIVersion: "test.example.com/v1", Kind: "Widget"},
		// Look-alike suffixes must not match.
		{APIVersion: "fluxcd.io/v1", Kind: "Fake"},
		{APIVersion: "notfluxcd.io/v1", Kind: "Fake"},
	}
	for _, k := range notFlux {
		if IsFluxKind(k) {
			t.Errorf("IsFluxKind(%s) = true, want false", k.GroupVersionKind())
		}
	}
}
