package helm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/chart"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/repo/v1/repotest"

	"github.com/banschikovde/fluxview/internal/flux"
)

// writeTgzChartAt (shared with inflater_test.go) writes the archive; the OCI
// test server requires it to expand into oci-dependent-chart/.

// ociTestEnv is a local OCI registry (helm's repotest, htpasswd-protected,
// plain HTTP on 127.0.0.1) with one pushed chart.
type ociTestEnv struct {
	srv    *repotest.OCIServer
	ref    string // oci://<registry>/u/ocitestuser/oci-dependent-chart (no tag)
	digest string // manifest digest of the pushed chart
}

// newOCITestEnv pushes a minimal chart as oci-dependent-chart:0.1.0. The
// registry requires basic auth username/password — which lets tests prove
// "zero network" by calling with deliberately wrong credentials: any request
// would fail with 401.
func newOCITestEnv(t *testing.T) *ociTestEnv {
	t.Helper()
	dir := t.TempDir()
	writeTgzChartAt(t, filepath.Join(dir, "oci-dependent-chart-0.1.0.tgz"), "oci-dependent-chart", map[string]string{
		"Chart.yaml":  "apiVersion: v2\nname: oci-dependent-chart\nversion: 0.1.0\n",
		"values.yaml": "replicas: \"1\"\n",
		"templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: snap\n" +
			"data:\n  replicas: {{ .Values.replicas | quote }}\n",
	})

	srv, err := repotest.NewOCIServer(t, dir)
	if err != nil {
		t.Fatalf("NewOCIServer: %v", err)
	}
	res := srv.RunWithReturn(t)
	return &ociTestEnv{
		srv:    srv,
		ref:    "oci://" + srv.RegistryURL + "/u/ocitestuser/oci-dependent-chart",
		digest: res.PushedChart.Manifest.Digest,
	}
}

// TestResolveOCIChart_ColdThenWarmOffline: the first resolution contacts the
// registry (tag→digest manifest + chart blob); the second performs zero
// network requests, proven by wrong credentials that would 401 on any request.
func TestResolveOCIChart_ColdThenWarmOffline(t *testing.T) {
	env := newOCITestEnv(t)
	in, err := NewInflater(WithCacheDir(t.TempDir()), WithIndexTTL(time.Hour))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}

	cold, err := in.resolveOCIChart(env.ref, "0.1.0", "username", "password")
	if err != nil {
		t.Fatalf("cold resolve: %v", err)
	}
	ch, err := loader.Load(cold)
	if err != nil {
		t.Fatalf("loading cached chart: %v", err)
	}
	acc, err := chart.NewDefaultAccessor(ch)
	if err != nil {
		t.Fatalf("chart accessor: %v", err)
	}
	if acc.Name() != "oci-dependent-chart" {
		t.Errorf("unexpected chart name %q", acc.Name())
	}

	// Warm with wrong credentials: any network request would fail with 401.
	warm, err := in.resolveOCIChart(env.ref, "0.1.0", "wrong", "creds")
	if err != nil {
		t.Fatalf("warm resolve must not touch the network: %v", err)
	}
	if warm != cold {
		t.Errorf("warm resolve path %q != cold %q", warm, cold)
	}

	// The tag→digest mapping is persisted.
	data, err := os.ReadFile(in.ociDigestsPath)
	if err != nil {
		t.Fatalf("reading oci-digests.yaml: %v", err)
	}
	if !strings.Contains(string(data), env.digest) {
		t.Errorf("oci-digests.yaml does not contain %s:\n%s", env.digest, string(data))
	}
}

// TestResolveOCIChart_DigestPinnedNeverResolves: a digest-pinned ref is
// immutable — it downloads the blob once and never asks the registry to
// resolve anything again (no tag→digest entry is even written).
func TestResolveOCIChart_DigestPinnedNeverResolves(t *testing.T) {
	env := newOCITestEnv(t)
	cacheDir := t.TempDir()

	in, err := NewInflater(WithCacheDir(cacheDir), WithIndexTTL(time.Hour))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	pinned := env.ref + "@" + env.digest

	cold, err := in.resolveOCIChart(pinned, "", "username", "password")
	if err != nil {
		t.Fatalf("cold digest-pinned resolve: %v", err)
	}

	// Fresh Inflater (new process), wrong creds, TTL=0: nothing to refresh,
	// nothing to resolve — the blob comes from the content cache.
	in2, err := NewInflater(WithCacheDir(cacheDir), WithIndexTTL(0))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	warm, err := in2.resolveOCIChart(pinned, "", "wrong", "creds")
	if err != nil {
		t.Fatalf("warm digest-pinned resolve must not touch the network: %v", err)
	}
	if warm != cold {
		t.Errorf("warm resolve path %q != cold %q", warm, cold)
	}

	if _, err := os.Stat(filepath.Join(cacheDir, "oci-digests.yaml")); !os.IsNotExist(err) {
		t.Errorf("digest-pinned refs must not write oci-digests.yaml")
	}
}

// TestResolveOCIChart_TTLZeroFallsBackToStaleDigest: with TTL=0 the tag is
// re-resolved every time; when the registry rejects the request but a cached
// digest exists, it is reused with a warning (offline resilience, mirroring
// the repo-index fallback).
func TestResolveOCIChart_TTLZeroFallsBackToStaleDigest(t *testing.T) {
	env := newOCITestEnv(t)
	cacheDir := t.TempDir()

	warmup, err := NewInflater(WithCacheDir(cacheDir), WithIndexTTL(time.Hour))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	if _, err := warmup.resolveOCIChart(env.ref, "0.1.0", "username", "password"); err != nil {
		t.Fatalf("warmup resolve: %v", err)
	}

	in, err := NewInflater(WithCacheDir(cacheDir), WithIndexTTL(0))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	stderr := captureStderrHelm(func() {
		// TTL=0 forces a re-resolve; wrong creds make it fail with 401, the
		// stale digest must carry the run.
		if _, err := in.resolveOCIChart(env.ref, "0.1.0", "wrong", "creds"); err != nil {
			t.Fatalf("stale-digest fallback: %v", err)
		}
	})
	if !strings.Contains(stderr, "using cached") {
		t.Errorf("expected stale-digest warning, got:\n%s", stderr)
	}
}

// TestResolveOCIChart_ResolveFailureNoCache: no cached digest + unreachable
// credentials → a real error, not a silent skip.
func TestResolveOCIChart_ResolveFailureNoCache(t *testing.T) {
	env := newOCITestEnv(t)
	in, err := NewInflater(WithCacheDir(t.TempDir()), WithIndexTTL(time.Hour))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	_, err = in.resolveOCIChart(env.ref, "0.1.0", "wrong", "creds")
	if err == nil {
		t.Fatal("expected error when tag cannot be resolved and no cache exists")
	}
	if !strings.Contains(err.Error(), "resolving ") {
		t.Errorf("error %q does not mention the failed resolution", err.Error())
	}
}

// TestInflateHelmRelease_OCICachedAcrossRuns: end-to-end — two Inflaters
// (processes) sharing a cache dir; the second renders with wrong credentials,
// proving the warm path is fully offline.
func TestInflateHelmRelease_OCICachedAcrossRuns(t *testing.T) {
	env := newOCITestEnv(t)
	cacheDir := t.TempDir()
	hr := flux.HelmRelease{
		Metadata: flux.ObjectMeta{Name: "app", Namespace: "test"},
		Spec: flux.HelmReleaseSpec{
			Chart: flux.HelmReleaseChart{Spec: flux.HelmReleaseChartSpec{
				Chart:   env.ref,
				Version: "0.1.0",
			}},
		},
	}

	in1, err := NewInflater(WithCacheDir(cacheDir), WithIndexTTL(time.Hour))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	out1, err := in1.InflateHelmRelease(context.Background(), hr, "", "username", "password", nil, nil, "")
	if err != nil {
		t.Fatalf("cold InflateHelmRelease: %v", err)
	}
	if !strings.Contains(string(out1), `replicas: "1"`) {
		t.Errorf("cold render missing chart output:\n%s", string(out1))
	}

	in2, err := NewInflater(WithCacheDir(cacheDir), WithIndexTTL(time.Hour))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	out2, err := in2.InflateHelmRelease(context.Background(), hr, "", "wrong", "creds", nil, nil, "")
	if err != nil {
		t.Fatalf("warm InflateHelmRelease must not touch the network: %v", err)
	}
	if string(out1) != string(out2) {
		t.Errorf("warm render differs from cold:\ncold:\n%s\nwarm:\n%s", string(out1), string(out2))
	}
}

// pushTag publishes the same chart under an additional tag, for tests of
// floating-version resolution (empty version / semver constraints). The
// archive's Chart.yaml version matches the tag: the registry client's strict
// mode rejects pushes where they differ.
func (e *ociTestEnv) pushTag(t *testing.T, tag string) {
	t.Helper()
	tgz := filepath.Join(t.TempDir(), "chart.tgz")
	writeTgzChartAt(t, tgz, "oci-dependent-chart", map[string]string{
		"Chart.yaml":        fmt.Sprintf("apiVersion: v2\nname: oci-dependent-chart\nversion: %s\n", tag),
		"values.yaml":       "replicas: \"1\"\n",
		"templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: snap\ndata:\n  replicas: {{ .Values.replicas | quote }}\n",
	})
	data, err := os.ReadFile(tgz)
	if err != nil {
		t.Fatalf("reading chart archive: %v", err)
	}
	ref := fmt.Sprintf("%s/u/ocitestuser/oci-dependent-chart:%s", e.srv.RegistryURL, tag)
	if _, err := e.srv.Client.Push(data, ref); err != nil {
		t.Fatalf("pushing tag %s: %v", tag, err)
	}
}

// resolveOCIChart must accept the same floating versions the SDK's
// ValidateReference supported: empty version selects the highest semver tag,
// a semver range selects the highest match (OCIRepository spec.ref.semver).
func TestResolveOCIChart_FloatingVersions(t *testing.T) {
	env := newOCITestEnv(t)
	env.pushTag(t, "0.1.1")
	env.pushTag(t, "0.2.0")

	tests := []struct {
		name    string
		version string
		wantTag string
	}{
		{"empty version selects highest", "", "0.2.0"},
		{"semver range selects highest match", "0.1.x", "0.1.1"},
		{"range with floor", ">=0.2.0", "0.2.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, err := NewInflater(WithCacheDir(t.TempDir()), WithIndexTTL(time.Hour))
			if err != nil {
				t.Fatalf("NewInflater: %v", err)
			}
			if _, err := in.resolveOCIChart(env.ref, tt.version, "username", "password"); err != nil {
				t.Fatalf("resolveOCIChart(version=%q): %v", tt.version, err)
			}
			wantKey := env.srv.RegistryURL + "/u/ocitestuser/oci-dependent-chart:" + tt.wantTag
			if _, ok := in.ociDigests[wantKey]; !ok {
				t.Errorf("version %q: no digest cached under selected tag %s (have %v)", tt.version, tt.wantTag, in.ociDigests)
			}
			wantSel := env.srv.RegistryURL + "/u/ocitestuser/oci-dependent-chart:" + tt.version
			if got := in.ociTags[wantSel].Tag; got != tt.wantTag {
				t.Errorf("version %q: cached selection %q, want %q", tt.version, got, tt.wantTag)
			}
		})
	}
}

// TestResolveOCIChart_FloatingWarmOffline: a floating version is fully cached
// too — the second run selects the tag and digest from the caches with zero
// registry requests (wrong credentials would 401 on any request).
func TestResolveOCIChart_FloatingWarmOffline(t *testing.T) {
	env := newOCITestEnv(t)
	env.pushTag(t, "0.2.0")
	cacheDir := t.TempDir()

	warmup, err := NewInflater(WithCacheDir(cacheDir), WithIndexTTL(time.Hour))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	cold, err := warmup.resolveOCIChart(env.ref, "", "username", "password")
	if err != nil {
		t.Fatalf("cold resolve: %v", err)
	}

	in, err := NewInflater(WithCacheDir(cacheDir), WithIndexTTL(time.Hour))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	warm, err := in.resolveOCIChart(env.ref, "", "wrong", "creds")
	if err != nil {
		t.Fatalf("warm floating resolve must not touch the network: %v", err)
	}
	if warm != cold {
		t.Errorf("warm path %q != cold %q", warm, cold)
	}
}

// TestResolveOCIChart_TTLZeroStaleTagSelection: TTL=0 forces re-listing tags;
// when that fails (401) but a cached selection exists, it is reused with a
// warning — same offline contract as indexes and digests.
func TestResolveOCIChart_TTLZeroStaleTagSelection(t *testing.T) {
	env := newOCITestEnv(t)
	env.pushTag(t, "0.2.0")
	cacheDir := t.TempDir()

	warmup, err := NewInflater(WithCacheDir(cacheDir), WithIndexTTL(time.Hour))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	if _, err := warmup.resolveOCIChart(env.ref, "", "username", "password"); err != nil {
		t.Fatalf("warmup resolve: %v", err)
	}

	in, err := NewInflater(WithCacheDir(cacheDir), WithIndexTTL(0))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	stderr := captureStderrHelm(func() {
		if _, err := in.resolveOCIChart(env.ref, "", "wrong", "creds"); err != nil {
			t.Fatalf("stale tag-selection fallback: %v", err)
		}
	})
	if !strings.Contains(stderr, "using cached selection") {
		t.Errorf("expected stale-selection warning, got:\n%s", stderr)
	}
}

// TestResolveOCIChart_ConstraintNoMatch: a constraint no tag satisfies is a
// hard error, not a silent skip.
func TestResolveOCIChart_ConstraintNoMatch(t *testing.T) {
	env := newOCITestEnv(t)

	in, err := NewInflater(WithCacheDir(t.TempDir()), WithIndexTTL(time.Hour))
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	_, err = in.resolveOCIChart(env.ref, "9.9.x", "username", "password")
	if err == nil {
		t.Fatal("expected error for unsatisfiable constraint")
	}
	if !strings.Contains(err.Error(), "selecting tag") {
		t.Errorf("error %q does not mention tag selection", err.Error())
	}
}

// TestParseOCIRef covers the reference forms the resolver accepts.
func TestParseOCIRef(t *testing.T) {
	tests := []struct {
		ref    string
		base   string
		tag    string
		digest string
	}{
		{"oci://ghcr.io/vm/helm-charts/chart:1.2.3", "ghcr.io/vm/helm-charts/chart", "1.2.3", ""},
		{"oci://ghcr.io/vm/helm-charts/chart:1.2.3@sha256:abc", "ghcr.io/vm/helm-charts/chart", "1.2.3", "sha256:abc"},
		{"oci://ghcr.io/vm/helm-charts/chart@sha256:abc", "ghcr.io/vm/helm-charts/chart", "", "sha256:abc"},
		{"oci://localhost:5000/charts/app", "localhost:5000/charts/app", "", ""},
	}
	for _, tt := range tests {
		got := parseOCIRef(tt.ref)
		if got.base != tt.base || got.tag != tt.tag || got.digest != tt.digest {
			t.Errorf("parseOCIRef(%q) = %+v, want base=%q tag=%q digest=%q", tt.ref, got, tt.base, tt.tag, tt.digest)
		}
	}
}

// TestOCIPlainHTTP: loopback registries are plain HTTP, public ones are not.
func TestOCIPlainHTTP(t *testing.T) {
	for _, tt := range []struct {
		ref  string
		want bool
	}{
		{"oci://localhost:5000/charts/app", true},
		{"oci://127.0.0.1:5000/charts/app:1.0", true},
		{"oci://ghcr.io/vm/charts/app:1.0", false},
		{"oci://quay.io/jetstack/charts/cert-manager:1.2.3", false},
	} {
		if got := ociPlainHTTP(tt.ref); got != tt.want {
			t.Errorf("ociPlainHTTP(%q) = %v, want %v", tt.ref, got, tt.want)
		}
	}
}
