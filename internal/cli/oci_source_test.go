package cli

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/banschikovde/fluxview/internal/ocisource/ocisourcetest"
)

// chartLayerMediaType is the Envoy-style layerSelector: the OCIRepository
// points at a helm chart artifact and takes its chart layer.
const chartLayerMediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

// ociFleetFixture is a fleet repository whose Kustomization sources its
// content from an OCIRepository artifact in a local htpasswd registry —
// the issue #21 layout.
type ociFleetFixture struct {
	reg        *ocisourcetest.Registry
	repoRoot   string
	clusterDir string
}

// dockerConfigCLI points DOCKER_CONFIG at a config holding the test
// registry's credentials — the pull-side analog of `docker login`.
func dockerConfigCLI(t *testing.T, reg *ocisourcetest.Registry) {
	t.Helper()
	dir := t.TempDir()
	enc := base64.StdEncoding.EncodeToString([]byte(reg.Username + ":" + reg.Password))
	config := `{"auths": {"` + reg.URL + `": {"auth": "` + enc + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0o600); err != nil {
		t.Fatalf("writing docker config: %v", err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
}

// newOCIFleetFixture lays out the cluster tree; pushEnvoyArtifact adds
// artifact versions.
func newOCIFleetFixture(t *testing.T) *ociFleetFixture {
	t.Helper()
	reg := ocisourcetest.NewRegistry(t)
	dockerConfigCLI(t, reg)

	repoRoot := t.TempDir()
	gitInit(t, repoRoot)
	return &ociFleetFixture{
		reg:        reg,
		repoRoot:   repoRoot,
		clusterDir: filepath.Join(repoRoot, "clusters", "test"),
	}
}

// pushEnvoy pushes one envoy-style artifact version: a helm chart layer
// whose crds subtree is the Kustomization content.
func (f *ociFleetFixture) pushEnvoy(t *testing.T, tag, contentMarker string) ocisourcetest.Artifact {
	t.Helper()
	return f.reg.PushArtifact(t, "u/envoy", tag, chartLayerMediaType, map[string]string{
		"gateway-helm/Chart.yaml":                          "apiVersion: v2\nname: gateway-helm\nversion: " + tag + "\n",
		"gateway-helm/charts/crds/crds/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - crd.yaml\n",
		"gateway-helm/charts/crds/crds/crd.yaml":           "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: " + contentMarker + "\n",
	})
}

// writeKS writes the OCIRepository + Kustomization manifests; refYAML is
// the spec.ref block ("" for none) and selectorYAML the layerSelector
// block ("" for none).
func (f *ociFleetFixture) writeKS(t *testing.T, url, refYAML, selectorYAML string) {
	t.Helper()
	refBlock := ""
	if refYAML != "" {
		refBlock = "  ref:\n" + refYAML
	}
	selectorBlock := ""
	if selectorYAML != "" {
		selectorBlock = "  layerSelector:\n" + selectorYAML
	}
	writeHelper(t, f.clusterDir, "envoy.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: envoy-gateway
  namespace: envoy-gateway-system
spec:
  interval: 6h
  url: `+url+`
`+refBlock+selectorBlock+`---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: envoy-gateway-crds
  namespace: envoy-gateway-system
spec:
  interval: 24h
  path: ./gateway-helm/charts/crds/crds
  prune: false
  sourceRef:
    kind: OCIRepository
    name: envoy-gateway
`)
}

// ks flags with an isolated OCI source cache.
func (f *ociFleetFixture) buildFlags() *BuildFlags {
	return &BuildFlags{
		Path: f.clusterDir,
		KsCache: kustomizeCacheOptions{
			gitSourceDir: filepath.Join(f.repoRoot, ".cache-test-git"),
			gitSourceTtl: time.Hour,
			ociSourceDir: filepath.Join(f.repoRoot, ".cache-test-oci"),
			ociSourceTtl: time.Hour,
		},
	}
}

// TestBuildKS_OCIArtifactSource is the issue #21 acceptance: a
// Kustomization sourced from an OCIRepository builds its content from the
// fetched artifact — no warnings, CRDs from the chart layer in the
// output — across every ref form.
func TestBuildKS_OCIArtifactSource(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refYAML string
		marker  string
	}{
		{name: "tag", refYAML: "    tag: v1.10.0\n", marker: "from-artifact-v1-10-0"},
		{name: "digest", refYAML: "    digest: DIGEST\n", marker: "from-artifact-v1-10-0"},
		{name: "semver", refYAML: "    semver: \">=1.0.0 <2.0.0\"\n", marker: "from-artifact-v1-10-0"},
		{name: "no ref (literal latest)", refYAML: "", marker: "from-artifact-latest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOCIFleetFixture(t)
			f.pushEnvoy(t, "v1.9.0", "from-artifact-v1-9-0")
			art := f.pushEnvoy(t, "v1.10.0", tc.marker)
			f.pushEnvoy(t, "latest", "from-artifact-latest")

			refYAML := tc.refYAML
			if tc.name == "digest" {
				refYAML = "    digest: " + art.Digest + "\n"
			}
			f.writeKS(t, "oci://"+f.reg.URL+"/u/envoy", refYAML,
				"    mediaType: "+chartLayerMediaType+"\n    operation: copy\n")

			var runErr error
			var stderr string
			stdout := captureStdout(func() {
				stderr = captureStderr(func() {
					runErr = runBuild(context.Background(), []string{"ks"}, f.buildFlags())
				})
			})
			if runErr != nil {
				t.Fatalf("build ks from an OCI artifact must succeed, got: %v\nstderr:\n%s", runErr, stderr)
			}
			if !strings.Contains(stdout, tc.marker) {
				t.Errorf("output must contain the artifact content %q, got:\n%s", tc.marker, stdout)
			}
			if strings.Contains(stderr, "Warning") {
				t.Errorf("a fetched artifact must build without warnings, got:\n%s", stderr)
			}
		})
	}
}

// TestBuildKS_OCIArtifact_SourceFirst: a same-named local directory is
// different content that cannot be told apart locally — the artifact wins,
// exactly like an external GitRepository upstream.
func TestBuildKS_OCIArtifact_SourceFirst(t *testing.T) {
	f := newOCIFleetFixture(t)
	f.pushEnvoy(t, "v1.10.0", "from-artifact-v1-10-0")
	f.writeKS(t, "oci://"+f.reg.URL+"/u/envoy", "    tag: v1.10.0\n", "")

	// The local trap: same path, different content.
	writeHelper(t, filepath.Join(f.repoRoot, "gateway-helm", "charts", "crds", "crds"), "cm.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: local-trap\n")
	writeHelper(t, filepath.Join(f.repoRoot, "gateway-helm", "charts", "crds", "crds"), "kustomization.yaml",
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - cm.yaml\n")

	var runErr error
	var stderr string
	stdout := captureStdout(func() {
		stderr = captureStderr(func() {
			runErr = runBuild(context.Background(), []string{"ks"}, f.buildFlags())
		})
	})
	if runErr != nil {
		t.Fatalf("build ks: %v\nstderr:\n%s", runErr, stderr)
	}
	if !strings.Contains(stdout, "from-artifact-v1-10-0") {
		t.Errorf("source-first: the artifact content must win, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "local-trap") {
		t.Errorf("the same-named local directory must not leak into the output, got:\n%s", stdout)
	}
}

// TestBuildKS_OCIArtifact_LocalOverride: FLUXVIEW_OCI_LOCAL_SOURCES names
// artifacts of this very repository — their paths resolve locally and no
// fetch happens (here proven by an unreachable registry URL).
func TestBuildKS_OCIArtifact_LocalOverride(t *testing.T) {
	f := newOCIFleetFixture(t)
	f.writeKS(t, "oci://127.0.0.1:1/u/envoy", "    tag: v1.10.0\n", "")
	t.Setenv("FLUXVIEW_OCI_LOCAL_SOURCES", "envoy-gateway-system/envoy-gateway")

	writeHelper(t, filepath.Join(f.repoRoot, "gateway-helm", "charts", "crds", "crds"), "cm.yaml",
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: own-artifact\n")
	writeHelper(t, filepath.Join(f.repoRoot, "gateway-helm", "charts", "crds", "crds"), "kustomization.yaml",
		"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - cm.yaml\n")

	var runErr error
	var stderr string
	stdout := captureStdout(func() {
		stderr = captureStderr(func() {
			runErr = runBuild(context.Background(), []string{"ks"}, f.buildFlags())
		})
	})
	if runErr != nil {
		t.Fatalf("build ks over a local artifact must not touch the registry, got: %v\nstderr:\n%s", runErr, stderr)
	}
	if !strings.Contains(stdout, "own-artifact") {
		t.Errorf("the local artifact content must build, got:\n%s", stdout)
	}
	if strings.Contains(stderr, "fetching OCIRepository") {
		t.Errorf("a local-listed artifact must not be fetched, got:\n%s", stderr)
	}
}

// TestValidate_OCIArtifact_MissingPath: a path absent from the fetched
// artifact is a real configuration error — the gate fails, exactly like a
// missing path in a fetched external clone.
func TestValidate_OCIArtifact_MissingPath(t *testing.T) {
	f := newOCIFleetFixture(t)
	f.pushEnvoy(t, "v1.10.0", "from-artifact-v1-10-0")
	// The KS path points into the artifact, but at a subtree that is not
	// there.
	f.writeKS(t, "oci://"+f.reg.URL+"/u/envoy", "    tag: v1.10.0\n", "")
	manifestPath := filepath.Join(f.clusterDir, "envoy.yaml")
	original, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	patched := strings.Replace(string(original), "./gateway-helm/charts/crds/crds", "./gateway-helm/charts/crds/absent", 1)
	if err := os.WriteFile(manifestPath, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}

	flags := &ValidateFlags{
		Path:                  f.clusterDir,
		disableDefaultSchemas: true,
		KsCache: kustomizeCacheOptions{
			ociSourceDir: filepath.Join(f.repoRoot, ".cache-test-oci"),
			ociSourceTtl: time.Hour,
			gitSourceDir: filepath.Join(f.repoRoot, ".cache-test-git"),
			gitSourceTtl: time.Hour,
		},
	}
	var runErr error
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), flags)
	})
	if runErr == nil {
		t.Fatalf("a path missing from the artifact must fail the gate\nstderr:\n%s", stderr)
	}
	for _, want := range []string{"missing from the repository", "envoy-gateway-system/envoy-gateway-crds", "not found in external source OCIRepository envoy-gateway-system/envoy-gateway"} {
		if !strings.Contains(stderr, want) && !strings.Contains(runErr.Error(), want) {
			t.Errorf("expected %q in the failure, err: %v\nstderr:\n%s", want, runErr, stderr)
		}
	}
}

// TestValidate_OCIArtifact_BrokenManifest: a kustomize build failure
// inside the artifact fails the gate — partial validation must not pass.
func TestValidate_OCIArtifact_BrokenManifest(t *testing.T) {
	f := newOCIFleetFixture(t)
	f.reg.PushArtifact(t, "u/broken", "v1", ocisourcetest.DefaultLayerMediaType, map[string]string{
		"apps/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - broken.yaml\n",
		"apps/broken.yaml":        "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{{{  broken\n",
	})
	writeHelper(t, f.clusterDir, "broken.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: broken
  namespace: flux-system
spec:
  url: oci://`+f.reg.URL+`/u/broken
  ref:
    tag: v1
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: broken-app
  namespace: flux-system
spec:
  interval: 24h
  path: ./apps
  sourceRef:
    kind: OCIRepository
    name: broken
`)

	var runErr error
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:                  f.clusterDir,
			disableDefaultSchemas: true,
			KsCache: kustomizeCacheOptions{
				ociSourceDir: filepath.Join(f.repoRoot, ".cache-test-oci"),
				ociSourceTtl: time.Hour,
				gitSourceDir: filepath.Join(f.repoRoot, ".cache-test-git"),
				gitSourceTtl: time.Hour,
			},
		})
	})
	if runErr == nil {
		t.Fatalf("a broken manifest inside the artifact must fail the gate\nstderr:\n%s", stderr)
	}
	if !strings.Contains(runErr.Error(), "kustomize build failed") {
		t.Errorf("expected a build-failure gate error, got: %v\nstderr:\n%s", runErr, stderr)
	}
}

// TestBuildHR_HelmReleaseInsideArtifact: an HR discovered inside the
// fetched artifact inflates like a local one (its GitRepository-sourced
// chart resolves from the checkout).
func TestBuildHR_HelmReleaseInsideArtifact(t *testing.T) {
	f := newOCIFleetFixture(t)
	f.reg.PushArtifact(t, "u/hr", "v1", ocisourcetest.DefaultLayerMediaType, map[string]string{
		"apps/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - hr.yaml\n",
		"apps/hr.yaml": `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: inside-artifact
  namespace: apps
spec:
  chart:
    spec:
      chart: charts/my-chart
      version: "1.0.0"
      sourceRef:
        kind: GitRepository
        name: flux-system
`,
	})
	writeHelper(t, f.clusterDir, "hr-source.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: hr-source
  namespace: flux-system
spec:
  url: oci://`+f.reg.URL+`/u/hr
  ref:
    tag: v1
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: hr-app
  namespace: flux-system
spec:
  interval: 24h
  path: ./apps
  sourceRef:
    kind: OCIRepository
    name: hr-source
`)

	chartDir := filepath.Join(f.repoRoot, "charts", "my-chart")
	writeHelper(t, chartDir, "Chart.yaml", "apiVersion: v2\nname: my-chart\nversion: 1.0.0\n")
	writeHelper(t, chartDir, "values.yaml", "")
	writeHelper(t, filepath.Join(chartDir, "templates"), "dep.yaml", "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: from-local-chart\nspec:\n  replicas: 1\n")

	var runErr error
	var stderr string
	stdout := captureStdout(func() {
		stderr = captureStderr(func() {
			runErr = runBuild(context.Background(), []string{"hr"}, f.buildFlags())
		})
	})
	if runErr != nil {
		t.Fatalf("build hr with an HR inside the artifact must succeed, got: %v\nstderr:\n%s", runErr, stderr)
	}
	if !strings.Contains(stdout, "from-local-chart") {
		t.Errorf("the HR from inside the artifact must inflate, got:\n%s", stdout)
	}
}

// TestDiffHR_HelmReleaseInsideArtifact: the comparison side fetches the
// same artifact (shared cache), so the HR is diffed, not reported as
// added/removed.
func TestDiffHR_HelmReleaseInsideArtifact(t *testing.T) {
	f := newOCIFleetFixture(t)
	f.reg.PushArtifact(t, "u/hr", "v1", ocisourcetest.DefaultLayerMediaType, map[string]string{
		"apps/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - hr.yaml\n",
		"apps/hr.yaml": `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: inside-artifact
  namespace: apps
spec:
  chart:
    spec:
      chart: charts/my-chart
      version: "1.0.0"
      sourceRef:
        kind: GitRepository
        name: flux-system
`,
	})
	writeHelper(t, f.clusterDir, "hr-source.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: hr-source
  namespace: flux-system
spec:
  url: oci://`+f.reg.URL+`/u/hr
  ref:
    tag: v1
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: hr-app
  namespace: flux-system
spec:
  interval: 24h
  path: ./apps
  sourceRef:
    kind: OCIRepository
    name: hr-source
`)

	chartDir := filepath.Join(f.repoRoot, "charts", "my-chart")
	writeHelper(t, chartDir, "Chart.yaml", "apiVersion: v2\nname: my-chart\nversion: 1.0.0\n")
	writeHelper(t, chartDir, "values.yaml", "")
	writeHelper(t, filepath.Join(chartDir, "templates"), "dep.yaml", "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: from-local-chart\nspec:\n  replicas: 1\n")

	gitRun(t, f.repoRoot, "add", "-A")
	gitRun(t, f.repoRoot, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit", "-q", "-m", "v1")
	first := strings.TrimSpace(gitOutput(t, f.repoRoot, "rev-parse", "HEAD"))

	// Change the chart between the commits: the diff must attribute the
	// change to the HR discovered inside the artifact on both sides.
	writeHelper(t, filepath.Join(chartDir, "templates"), "dep.yaml", "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: from-local-chart\nspec:\n  replicas: 2\n")
	gitRun(t, f.repoRoot, "add", "-A")
	gitRun(t, f.repoRoot, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit", "-q", "-m", "v2")

	var runErr error
	var stderr string
	stdout := captureStdout(func() {
		stderr = captureStderr(func() {
			runErr = runDiff(context.Background(), []string{"hr"}, &DiffFlags{
				Path:       f.clusterDir,
				BranchOrig: first,
				Color:      "never",
				KsCache: kustomizeCacheOptions{
					gitSourceDir: filepath.Join(f.repoRoot, ".cache-test-git"),
					gitSourceTtl: time.Hour,
					ociSourceDir: filepath.Join(f.repoRoot, ".cache-test-oci"),
					ociSourceTtl: time.Hour,
				},
			})
		})
	})
	if runErr == nil {
		t.Fatalf("the chart change must surface as a diff (exit 1), stderr:\n%s", stderr)
	}
	if !strings.Contains(runErr.Error(), "differences found") {
		t.Errorf("expected a differences-found error, got: %v\nstdout:\n%s\nstderr:\n%s", runErr, stdout, stderr)
	}
	if !strings.Contains(stdout, "from-local-chart") {
		t.Errorf("the diff must show the artifact-discovered HR's deployment, got:\n%s", stdout)
	}
}

// TestBuildHR_OCIFetchFailure_WarnsInQuietDiscovery: an artifact that
// cannot be fetched warns even in the hr pipeline's quiet discovery — the
// fetch is the expected baseline now, silence would hide HelmReleases
// vanishing from the output. The command stays lenient (exit 0).
func TestBuildHR_OCIFetchFailure_WarnsInQuietDiscovery(t *testing.T) {
	f := newOCIFleetFixture(t)
	f.writeKS(t, "oci://127.0.0.1:1/u/envoy", "    tag: v1.10.0\n", "")

	var runErr error
	var stderr string
	_ = captureStdout(func() {
		stderr = captureStderr(func() {
			runErr = runBuild(context.Background(), []string{"hr"}, f.buildFlags())
		})
	})
	if runErr != nil {
		t.Fatalf("build hr must stay lenient on a failed artifact fetch, got: %v\nstderr:\n%s", runErr, stderr)
	}
	if !strings.Contains(stderr, "fetching OCIRepository envoy-gateway-system/envoy-gateway for envoy-gateway-system/envoy-gateway-crds failed") {
		t.Errorf("the fetch failure must warn in hr mode too, got:\n%s", stderr)
	}
}

// TestValidate_OCIFetchFailure_UncheckedAndPasses: an unreachable registry
// is best-effort for the gate — the Kustomization is named "left
// unchecked", the local tree still validates, exit 0.
func TestValidate_OCIFetchFailure_UncheckedAndPasses(t *testing.T) {
	f := newOCIFleetFixture(t)
	f.writeKS(t, "oci://127.0.0.1:1/u/envoy", "    tag: v1.10.0\n", "")

	var runErr error
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:                  f.clusterDir,
			disableDefaultSchemas: true,
			KsCache: kustomizeCacheOptions{
				ociSourceDir: filepath.Join(f.repoRoot, ".cache-test-oci"),
				ociSourceTtl: time.Hour,
				gitSourceDir: filepath.Join(f.repoRoot, ".cache-test-git"),
				gitSourceTtl: time.Hour,
			},
		})
	})
	if runErr != nil {
		t.Fatalf("a failed artifact fetch must not fail the gate, got: %v\nstderr:\n%s", runErr, stderr)
	}
	if !strings.Contains(stderr, "left unchecked") || !strings.Contains(stderr, "envoy-gateway-system/envoy-gateway-crds") {
		t.Errorf("the gate summary must name the unchecked Kustomization, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "OCIRepository envoy-gateway-system/envoy-gateway") {
		t.Errorf("the unchecked cause must name the source, got:\n%s", stderr)
	}
}

// TestValidate_OCIArtifact_ValidContent_Passes pins the happy validate
// path: the artifact content reaches the checked set, no warnings.
func TestValidate_OCIArtifact_ValidContent_Passes(t *testing.T) {
	f := newOCIFleetFixture(t)
	f.pushEnvoy(t, "v1.10.0", "from-artifact-v1-10-0")
	f.writeKS(t, "oci://"+f.reg.URL+"/u/envoy", "    tag: v1.10.0\n", "")

	var runErr error
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:                  f.clusterDir,
			disableDefaultSchemas: true,
			KsCache: kustomizeCacheOptions{
				ociSourceDir: filepath.Join(f.repoRoot, ".cache-test-oci"),
				ociSourceTtl: time.Hour,
				gitSourceDir: filepath.Join(f.repoRoot, ".cache-test-git"),
				gitSourceTtl: time.Hour,
			},
		})
	})
	if runErr != nil {
		t.Fatalf("validate over a fetched artifact must pass, got: %v\nstderr:\n%s", runErr, stderr)
	}
	if strings.Contains(stderr, "left unchecked") || strings.Contains(stderr, "Warning: fetching OCIRepository") {
		t.Errorf("a successful fetch must leave no warnings, got:\n%s", stderr)
	}
}

// TestOCIArtifact_EmptyPathIsArtifactRoot: a Kustomization on an
// OCIRepository without spec.path builds the artifact root — Flux's
// "absent path = source root". Local, git and kill-switch sources keep
// the historical empty-path behavior (separate follow-up).
func TestOCIArtifact_EmptyPathIsArtifactRoot(t *testing.T) {
	f := newOCIFleetFixture(t)
	f.reg.PushArtifact(t, "u/root", "v1", ocisourcetest.DefaultLayerMediaType, map[string]string{
		"kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - cm.yaml\n  - hr.yaml\n",
		"cm.yaml":            "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: from-artifact-root\n",
		"hr.yaml": `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: root-hr
  namespace: apps
spec:
  chart:
    spec:
      chart: charts/my-chart
      version: "1.0.0"
      sourceRef:
        kind: GitRepository
        name: flux-system
`,
	})
	writeHelper(t, f.clusterDir, "root-source.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: root-source
  namespace: flux-system
spec:
  url: oci://`+f.reg.URL+`/u/root
  ref:
    tag: v1
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: root-app
  namespace: flux-system
spec:
  interval: 24h
  sourceRef:
    kind: OCIRepository
    name: root-source
`)
	chartDir := filepath.Join(f.repoRoot, "charts", "my-chart")
	writeHelper(t, chartDir, "Chart.yaml", "apiVersion: v2\nname: my-chart\nversion: 1.0.0\n")
	writeHelper(t, chartDir, "values.yaml", "")
	writeHelper(t, filepath.Join(chartDir, "templates"), "dep.yaml", "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: from-local-chart\nspec:\n  replicas: 1\n")

	// build ks: the artifact root's resources are in the output.
	var runErr error
	var stderr string
	stdout := captureStdout(func() {
		stderr = captureStderr(func() {
			runErr = runBuild(context.Background(), []string{"ks"}, f.buildFlags())
		})
	})
	if runErr != nil {
		t.Fatalf("build ks over an empty-path OCI Kustomization must succeed, got: %v\nstderr:\n%s", runErr, stderr)
	}
	if !strings.Contains(stdout, "from-artifact-root") {
		t.Errorf("the artifact root content must build, got:\n%s", stdout)
	}

	// build hr: a HelmRelease at the artifact root inflates.
	stdout = captureStdout(func() {
		stderr = captureStderr(func() {
			runErr = runBuild(context.Background(), []string{"hr"}, f.buildFlags())
		})
	})
	if runErr != nil {
		t.Fatalf("build hr must discover the artifact-root HelmRelease, got: %v\nstderr:\n%s", runErr, stderr)
	}
	if !strings.Contains(stdout, "from-local-chart") {
		t.Errorf("the artifact-root HelmRelease must inflate, got:\n%s", stdout)
	}

	// validate: the artifact root content reaches the checked set.
	stderr = captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:                  f.clusterDir,
			disableDefaultSchemas: true,
			KsCache: kustomizeCacheOptions{
				ociSourceDir: filepath.Join(f.repoRoot, ".cache-test-oci"),
				ociSourceTtl: time.Hour,
				gitSourceDir: filepath.Join(f.repoRoot, ".cache-test-git"),
				gitSourceTtl: time.Hour,
			},
		})
	})
	if runErr != nil {
		t.Fatalf("validate must pass over the artifact root, got: %v\nstderr:\n%s", runErr, stderr)
	}
	if strings.Contains(stderr, "left unchecked") || strings.Contains(stderr, "Warning: fetching") {
		t.Errorf("a successful empty-path fetch must leave no warnings, got:\n%s", stderr)
	}
}

// TestOCI_MissingRepositoryObject_DistinctCause: with fetching on, a
// Kustomization whose OCIRepository manifest is nowhere in the repository
// reports "not found in the repository"; with the kill switch the cause
// stays "not fetched". Both leave the KS unchecked by validate.
func TestOCI_MissingRepositoryObject_DistinctCause(t *testing.T) {
	for _, tc := range []struct {
		name      string
		noFetch   bool
		wantCause string
	}{
		{name: "fetch on names the missing object", noFetch: false, wantCause: "OCIRepository envoy-gateway-system/envoy-gateway not found in the repository"},
		{name: "kill switch keeps the not-fetched cause", noFetch: true, wantCause: "source kind OCIRepository is not fetched"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOCIFleetFixture(t)
			// No OCIRepository manifest anywhere: only the KS.
			writeHelper(t, f.clusterDir, "orphan.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: envoy-gateway-crds
  namespace: envoy-gateway-system
spec:
  interval: 24h
  path: ./gateway-helm/charts/crds/crds
  sourceRef:
    kind: OCIRepository
    name: envoy-gateway
`)
			flags := &ValidateFlags{
				Path:                  f.clusterDir,
				disableDefaultSchemas: true,
				NoOCISourceFetch:      tc.noFetch,
				KsCache: kustomizeCacheOptions{
					ociSourceDir: filepath.Join(f.repoRoot, ".cache-test-oci"),
					ociSourceTtl: time.Hour,
					gitSourceDir: filepath.Join(f.repoRoot, ".cache-test-git"),
					gitSourceTtl: time.Hour,
				},
			}
			var runErr error
			stderr := captureStderr(func() {
				runErr = runValidate(context.Background(), flags)
			})
			if runErr != nil {
				t.Fatalf("validate must stay lenient, got: %v\nstderr:\n%s", runErr, stderr)
			}
			if !strings.Contains(stderr, tc.wantCause) {
				t.Errorf("the cause must be %q, got:\n%s", tc.wantCause, stderr)
			}
			if !strings.Contains(stderr, "left unchecked") || !strings.Contains(stderr, "envoy-gateway-system/envoy-gateway-crds") {
				t.Errorf("the gate summary must still name the unchecked Kustomization, got:\n%s", stderr)
			}
		})
	}
}
