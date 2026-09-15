package validate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	kcresource "github.com/yannh/kubeconform/pkg/resource"
)

// DefaultSchemaRegistryURL is the default HTTP schema registry kubeconform
// fetches Kubernetes schemas from (the master branch of the
// kubernetes-json-schema fork, same as kubeconform's own "default"
// location).
const DefaultSchemaRegistryURL = "https://raw.githubusercontent.com/yannh/kubernetes-json-schema/master"

// Prefetch defaults: schemas are at most a few hundred kilobytes, so a
// generous per-request timeout only kicks in on a hung registry — the case
// these bounds exist for. Retries match kubeconform's retryablehttp setup.
const (
	// DefaultSchemaDownloadTimeout bounds one download attempt when the
	// caller does not configure one.
	DefaultSchemaDownloadTimeout = 30 * time.Second
	prefetchConcurrency          = 8
	prefetchRetries              = 2
	prefetchBackoff              = 250 * time.Millisecond
	prefetchMaxSchemaSize        = 64 << 20
)

// ResourceKind identifies a resource kind by apiVersion and kind.
type ResourceKind struct {
	APIVersion string
	Kind       string
}

// GroupVersionKind renders the apiVersion/Kind skip-list form
// ("apps/v1/Deployment"), matching kubeconform's skip matching.
func (k ResourceKind) GroupVersionKind() string {
	return k.APIVersion + "/" + k.Kind
}

// ResourceKinds parses a multi-doc YAML stream (build output) and returns
// the unique resource kinds in first-seen order. Documents without a usable
// apiVersion/kind are dropped: validation reports them as malformed.
func ResourceKinds(data []byte) []ResourceKind {
	var kinds []ResourceKind
	seen := make(map[ResourceKind]struct{})

	resources, _ := kcresource.FromStream(context.Background(), "input", bytes.NewReader(data))
	for res := range resources {
		sig, err := res.Signature()
		if err != nil || sig == nil || sig.Kind == "" || sig.Version == "" {
			continue
		}
		k := ResourceKind{APIVersion: sig.Version, Kind: sig.Kind}
		if _, dup := seen[k]; !dup {
			seen[k] = struct{}{}
			kinds = append(kinds, k)
		}
	}
	return kinds
}

// KindSuffix mirrors kubeconform's {{ .KindSuffix }} template value for an
// apiVersion: "-apps-v1" for "apps/v1", "-v1" for core "v1", "-helm-v2"
// for "helm.toolkit.fluxcd.io/v2" (first group label, then version).
func KindSuffix(apiVersion string) string {
	groupParts := strings.Split(apiVersion, "/")
	versionParts := strings.Split(groupParts[0], ".")
	suffix := "-" + strings.ToLower(versionParts[0])
	if len(groupParts) > 1 {
		suffix += "-" + strings.ToLower(groupParts[1])
	}
	return suffix
}

// SchemaFileName builds the schema file name kubeconform's local registry
// requests for a kind: lowercase kind + kind suffix
// ("Deployment", "apps/v1" → "deployment-apps-v1.json").
func SchemaFileName(kind, apiVersion string) string {
	return strings.ToLower(kind) + KindSuffix(apiVersion) + ".json"
}

// KindCoverage tells which kinds already have a local schema, so prefetch
// only fetches what the default registry would be asked for.
type KindCoverage struct {
	flatDirs       []string // dirs with <schema>.json at the root
	versionedRoots []string // kubernetes-json-schema checkout roots
	kubernetesVer  string   // normalized
	strict         bool
}

// NewKindCoverage builds coverage from the local schema dirs of a location
// list: flatDirs use the kubeconform local layout directly, versionedRoots
// use the v<version>-standalone[-strict]/ checkout layout for the given
// Kubernetes version.
func NewKindCoverage(kubernetesVersion string, strict bool, flatDirs, versionedRoots []string) KindCoverage {
	version := NormalizeKubernetesVersion(kubernetesVersion)
	if version == "" {
		version = DefaultKubernetesVersion
	}
	return KindCoverage{
		flatDirs:       flatDirs,
		versionedRoots: versionedRoots,
		kubernetesVer:  version,
		strict:         strict,
	}
}

// Covers reports whether a schema file for the kind exists in one of the
// local dirs.
func (c KindCoverage) Covers(kind ResourceKind) bool {
	name := SchemaFileName(kind.Kind, kind.APIVersion)
	for _, dir := range c.flatDirs {
		if fileExists(filepath.Join(dir, name)) {
			return true
		}
	}
	sub := filepath.Join(RegistryVersionDir(c.kubernetesVer, c.strict), name)
	for _, root := range c.versionedRoots {
		if fileExists(filepath.Join(root, sub)) {
			return true
		}
	}
	return false
}

// UncoveredKinds filters kinds down to those without a local schema and not
// excluded by --skip-kind: exactly the kinds kubeconform would ask the
// default registry for.
func UncoveredKinds(kinds []ResourceKind, coverage KindCoverage, skipKinds map[string]struct{}) []ResourceKind {
	var uncovered []ResourceKind
	for _, k := range kinds {
		if _, skipped := skipKinds[k.Kind]; skipped {
			continue
		}
		if _, skipped := skipKinds[k.GroupVersionKind()]; skipped {
			continue
		}
		if coverage.Covers(k) {
			continue
		}
		uncovered = append(uncovered, k)
	}
	return uncovered
}

// IsFluxKind reports whether a kind belongs to a Flux CRD group — every
// Flux toolkit group ends in .fluxcd.io (kustomize/source/helm/notification/
// image.toolkit.fluxcd.io). The default registry hosts only Kubernetes'
// own schemas and never carries these, so their 404 is guaranteed and
// carries no signal about --kubernetes-version.
func IsFluxKind(k ResourceKind) bool {
	group, _, _ := strings.Cut(k.APIVersion, "/")
	return strings.HasSuffix(group, ".fluxcd.io")
}

// PrefetchOptions configures PrefetchDefaultSchemas.
type PrefetchOptions struct {
	// KubernetesVersion selects the schema version (normalized; defaults
	// to DefaultKubernetesVersion).
	KubernetesVersion string
	// Strict selects the -strict schema variant of the registry.
	Strict bool
	// CacheDir stores downloaded schemas in the kubernetes-json-schema
	// layout; existing files are reused without a request.
	CacheDir string
	// BaseURL overrides the registry root (tests). Empty →
	// DefaultSchemaRegistryURL.
	BaseURL string
	// Client overrides the HTTP client (tests). Per-request timeouts are
	// applied on top of it via context.
	Client *http.Client
	// Concurrency caps parallel downloads (default 8).
	Concurrency int
	// RequestTimeout bounds one attempt: positive → the timeout, 0 →
	// DefaultSchemaDownloadTimeout, negative → no per-request timeout
	// (the caller's context still bounds the download).
	RequestTimeout time.Duration
	// Retries is the number of retries per schema after a failed attempt
	// (default 2, matching kubeconform's retryablehttp setup).
	Retries int
}

// PrefetchDefaultSchemas downloads the schemas for kinds from the default
// registry into CacheDir, laid out exactly like a kubernetes-json-schema
// checkout (v<version>-standalone[-strict]/<schema>.json). Kinds whose
// schema is already cached are not requested again.
//
// kubeconform's own HTTP loader has no timeout and takes no context, so a
// hung registry hangs validation forever; this prefetch runs the downloads
// under ctx with per-attempt timeouts and bounds retries, letting
// validation itself read everything from the local cache. A 404 is not an
// error (the kind has no schema in the registry); such kinds are returned
// as missing. Any other failure — after retries — is returned as an error
// so callers can fail closed.
func PrefetchDefaultSchemas(ctx context.Context, kinds []ResourceKind, opts PrefetchOptions) ([]ResourceKind, error) {
	baseURL := opts.BaseURL
	if baseURL == "" {
		baseURL = DefaultSchemaRegistryURL
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}}
	}
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = prefetchConcurrency
	}
	timeout := opts.RequestTimeout
	if timeout == 0 {
		timeout = DefaultSchemaDownloadTimeout
	} // negative: no per-request timeout, the context alone bounds requests
	retries := opts.Retries
	if retries <= 0 {
		retries = prefetchRetries
	}

	version := NormalizeKubernetesVersion(opts.KubernetesVersion)
	if version == "" {
		version = DefaultKubernetesVersion
	}
	versionDir := RegistryVersionDir(version, opts.Strict)

	var fetch []ResourceKind
	for _, k := range kinds {
		if !fileExists(filepath.Join(opts.CacheDir, versionDir, SchemaFileName(k.Kind, k.APIVersion))) {
			fetch = append(fetch, k)
		}
	}
	if len(fetch) == 0 {
		return nil, nil
	}

	if err := os.MkdirAll(filepath.Join(opts.CacheDir, versionDir), 0755); err != nil {
		return nil, fmt.Errorf("creating schema cache dir: %w", err)
	}

	missingByIndex := make([]bool, len(fetch))
	var mu sync.Mutex
	var firstErr error

	// Prefill a buffered queue: workers pull indexes off it, and a worker
	// that hit a hard error simply stops — the queue is drained or
	// abandoned, never deadlocked.
	work := make(chan int, len(fetch))
	for i := range fetch {
		work <- i
	}
	close(work)

	workers := concurrency
	if workers > len(fetch) {
		workers = len(fetch)
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range work {
				k := fetch[idx]
				notFound, err := fetchSchema(ctx, client, baseURL, versionDir, k, opts.CacheDir, timeout, retries)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				if notFound {
					missingByIndex[idx] = true
				}
				mu.Unlock()
				if err != nil {
					return
				}
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	err := firstErr
	mu.Unlock()
	if err != nil {
		return nil, err
	}

	var missing []ResourceKind
	for i, notFound := range missingByIndex {
		if notFound {
			missing = append(missing, fetch[i])
		}
	}
	return missing, nil
}

// fetchSchema downloads one schema with bounded retries. It returns
// notFound=true for a stable 404 (no retry — the registry has no schema
// for the kind) and an error for anything still failing after retries.
func fetchSchema(ctx context.Context, client *http.Client, baseURL, versionDir string, k ResourceKind, cacheDir string, timeout time.Duration, retries int) (notFound bool, err error) {
	name := SchemaFileName(k.Kind, k.APIVersion)
	url := baseURL + "/" + versionDir + "/" + name

	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * prefetchBackoff):
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}

		// A negative timeout disables the per-request limit: the request
		// then runs under the caller's context alone.
		reqCtx := ctx
		cancel := context.CancelFunc(func() {})
		if timeout > 0 {
			reqCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			return false, err
		}
		req.Header.Set("User-Agent", "fluxview")

		resp, err := client.Do(req)
		if err != nil {
			cancel()
			if reqCtx.Err() == nil && ctx.Err() != nil {
				return false, ctx.Err() // interrupted, not retryable
			}
			lastErr = fmt.Errorf("requesting %s: %w", url, err)
			continue
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, prefetchMaxSchemaSize))
		closeErr := resp.Body.Close()
		cancel()

		switch {
		case resp.StatusCode == http.StatusNotFound:
			return true, nil
		case resp.StatusCode == http.StatusOK && readErr == nil && closeErr == nil:
			// Cache only verified content: a truncated or non-JSON body
			// (CDN error page) would otherwise be pinned in the cache
			// forever and fail every later run.
			if len(body) >= prefetchMaxSchemaSize {
				return false, fmt.Errorf("schema %s exceeds the %d-byte limit", name, prefetchMaxSchemaSize)
			}
			if !json.Valid(body) {
				return false, fmt.Errorf("registry returned invalid JSON for %s", url)
			}
			if err := writeFileAtomic(filepath.Join(cacheDir, versionDir, name), body); err != nil {
				return false, fmt.Errorf("caching schema %s: %w", name, err)
			}
			return false, nil
		case resp.StatusCode == http.StatusOK:
			lastErr = fmt.Errorf("reading %s: %v / %v", url, readErr, closeErr)
		default:
			lastErr = fmt.Errorf("downloading %s: HTTP %d", url, resp.StatusCode)
		}
	}
	return false, lastErr
}

// RegistryVersionDir is the registry directory for a version: master uses
// its own alias, releases the v-prefixed form. Note that --strict applies
// to master as well — the registry serves master-standalone-strict exactly
// like v1.36.1-standalone-strict, mirroring kubeconform's template
// rendering (NormalizedKubernetesVersion + "-standalone" + StrictSuffix).
// An empty version falls back to DefaultKubernetesVersion, like the
// validator itself.
func RegistryVersionDir(kubernetesVersion string, strict bool) string {
	version := NormalizeKubernetesVersion(kubernetesVersion)
	if version == "" {
		version = DefaultKubernetesVersion
	}
	dir := "master-standalone"
	if version != "master" {
		dir = "v" + version + "-standalone"
	}
	if strict {
		dir += "-strict"
	}
	return dir
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// writeFileAtomic writes data to path via a temp file and rename, so a
// concurrent reader never sees a partial schema.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".prefetch-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = "" // renamed; nothing to clean up
	return nil
}
