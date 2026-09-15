package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// gitInit runs `git init` in dir to make it discoverable by FindRepoRoot.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v\n%s", dir, err, out)
	}
}

// TestRunValidate_RelativePath_NoWarnings is a regression test for the bug
// where `fluxview validate --path <relative>` produced spurious warnings of
// the form:
//
//	Warning: could not read cluster/ks.yaml: Rel: can't make cluster/ks.yaml
//	relative to /Users/…/repo
//
// Root cause: runValidate passed the raw (relative) clusterPath into
// flux.NewParser / resolveConfigMaps / buildKSContent instead of
// absClusterPath. The loose-file walker in buildKustomizeOverlays then
// called filepath.Rel(repoRoot, relativePath), which fails because the
// second argument isn't absolute.
//
// After the fix, all downstream calls receive absClusterPath and the
// loose-file walker resolves paths correctly.
func TestRunValidate_RelativePath_NoWarnings(t *testing.T) {
	repoRoot := t.TempDir()
	gitInit(t, repoRoot)

	// A cluster directory holding a Flux Kustomization CR.
	clusterDir := filepath.Join(repoRoot, "cluster")
	writeHelper(t, clusterDir, "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: app
  namespace: flux-system
spec:
  interval: 5m
  path: ./app
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
`)

	// The spec.path target with a native kustomization and one resource.
	appDir := filepath.Join(repoRoot, "app")
	writeHelper(t, appDir, "kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - configmap.yaml
`)
	writeHelper(t, appDir, "configmap.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  key: value
`)

	// A schema dir holding a local schema for the ConfigMap above, so the
	// run stays hermetic (no default HTTP schema location).
	schemaDir := filepath.Join(repoRoot, "schemas")
	if err := os.MkdirAll(schemaDir, 0755); err != nil {
		t.Fatalf("mkdir schema dir: %v", err)
	}
	writeHelper(t, schemaDir, "configmap-v1.json", `{"type": "object"}`)

	// Run validate from repoRoot with a RELATIVE --path. chdir back afterwards.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getcwd: %v", err)
	}
	defer func() { _ = os.Chdir(cwd) }()
	if err := os.Chdir(repoRoot); err != nil {
		t.Fatalf("chdir %s: %v", repoRoot, err)
	}

	flags := &ValidateFlags{
		Path:      "cluster", // relative on purpose
		SchemaDir: schemaDir,
		// Tests disable the default HTTP schema registry to stay offline:
		// the Flux Kustomization CR has no schema and is skipped, the
		// ConfigMap validates against the local schema.
		disableDefaultSchemas: true,
	}

	stderr := captureStderr(func() {
		_ = runValidate(context.Background(), flags)
	})

	if strings.Contains(stderr, "Rel: can't make") {
		t.Errorf("validate with relative --path produced spurious Rel warnings:\n%s", stderr)
	}
	// Sanity: the rest of the flow should still work (build runs, schemas
	// resolve, "All resources valid.").
	if !strings.Contains(stderr, "All resources valid.") {
		t.Errorf("expected 'All resources valid.' in stderr, got:\n%s", stderr)
	}
}

// TestRunValidate_BuildFailure_Fails pins the validation-gate contract: a
// failed kustomize build (here: malformed YAML in a resource file) must fail
// validate with ExitCodeError instead of silently validating the surviving
// subset and reporting "All resources valid." with exit 0. The per-dir
// warnings stay; the returned error names the failed paths.
func TestRunValidate_BuildFailure_Fails(t *testing.T) {
	repoRoot := t.TempDir()
	gitInit(t, repoRoot)

	clusterDir := filepath.Join(repoRoot, "cluster")
	writeHelper(t, clusterDir, "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: app
  namespace: flux-system
spec:
  interval: 5m
  path: ./app
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
`)

	// The spec.path target whose resource file is syntactically broken
	// (tab indentation) — kustomize build fails on it.
	appDir := filepath.Join(repoRoot, "app")
	writeHelper(t, appDir, "kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - configmap.yaml
`)
	writeHelper(t, appDir, "configmap.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app-config\ndata:\n\tkey: value\n")

	schemaDir := filepath.Join(repoRoot, "schemas")
	if err := os.MkdirAll(schemaDir, 0755); err != nil {
		t.Fatalf("mkdir schema dir: %v", err)
	}
	writeHelper(t, schemaDir, "configmap-v1.json", `{"type": "object"}`)

	flags := &ValidateFlags{
		Path:                  clusterDir,
		SchemaDir:             schemaDir,
		disableDefaultSchemas: true,
	}

	var runErr error
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), flags)
	})

	exitErr, ok := runErr.(*DiffExitError)
	if !ok {
		t.Fatalf("expected *DiffExitError, got %v", runErr)
	}
	if exitErr.ExitCode != ExitCodeError {
		t.Errorf("exit code = %d, want %d (ExitCodeError)", exitErr.ExitCode, ExitCodeError)
	}
	if !strings.Contains(exitErr.Error(), "kustomize build failed") {
		t.Errorf("error should report the failed build, got: %v", exitErr.Error())
	}
	if !strings.Contains(exitErr.Error(), appDir) {
		t.Errorf("error should name the failed path %s, got: %v", appDir, exitErr.Error())
	}
	// The detailed underlying error was already warned about once.
	if !strings.Contains(stderr, "Warning: kustomize build") {
		t.Errorf("expected the per-dir build warning in stderr, got:\n%s", stderr)
	}
	if strings.Contains(stderr, "All resources valid.") {
		t.Errorf("a failed build must not report 'All resources valid.', got:\n%s", stderr)
	}
}

// TestRunValidate_KubernetesJSONSchemaDir_Offline proves the full-offline
// contract: with a kubernetes-json-schema checkout inside --schema-dir
// and the default registry disabled, native kinds validate locally — no
// network, no new flags.
func TestRunValidate_KubernetesJSONSchemaDir_Offline(t *testing.T) {
	repoRoot := t.TempDir()
	gitInit(t, repoRoot)

	clusterDir := filepath.Join(repoRoot, "cluster")
	writeHelper(t, clusterDir, "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: app
  namespace: flux-system
spec:
  interval: 5m
  path: ./app
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
`)

	appDir := filepath.Join(repoRoot, "app")
	writeHelper(t, appDir, "kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - configmap.yaml
`)
	writeHelper(t, appDir, "configmap.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  key: 42
`)

	// A checkout layout with a schema for the default Kubernetes version:
	// ConfigMap data values must be strings.
	schemaDir := filepath.Join(repoRoot, "schemas")
	if err := os.MkdirAll(filepath.Join(schemaDir, "v1.36.1-standalone"), 0755); err != nil {
		t.Fatal(err)
	}
	writeHelper(t, schemaDir, "v1.36.1-standalone/configmap-v1.json", `{
	"type": "object",
	"properties": {
		"data": {"type": "object", "additionalProperties": {"type": "string"}}
	}
}`)

	flags := func() *ValidateFlags {
		return &ValidateFlags{
			Path:                  clusterDir,
			SchemaDir:             schemaDir,
			disableDefaultSchemas: true,
		}
	}

	// The integer data value fails against the local native-kind schema.
	var runErr error
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), flags())
	})
	exitErr, ok := runErr.(*DiffExitError)
	if !ok {
		t.Fatalf("expected *DiffExitError, got %v", runErr)
	}
	if exitErr.ExitCode != ExitValidationFailed {
		t.Errorf("exit code = %d, want %d (ExitValidationFailed)", exitErr.ExitCode, ExitValidationFailed)
	}
	if !strings.Contains(stderr, "ConfigMap app-config") {
		t.Errorf("expected the native-kind failure in stderr, got:\n%s", stderr)
	}

	// A version the checkout does not carry degrades gracefully: the
	// ConfigMap is skipped (no matching schema), not an error.
	writeHelper(t, appDir, "configmap.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  key: value
`)
	runErr = nil
	stderr = captureStderr(func() {
		f := flags()
		f.KubernetesVersion = "1.34.0"
		runErr = runValidate(context.Background(), f)
	})
	if runErr != nil {
		t.Fatalf("unexpected error for an uncovered version: %v", runErr)
	}
	if strings.Contains(stderr, "✗") {
		t.Errorf("an uncovered version should skip, not fail:\n%s", stderr)
	}
	if !strings.Contains(stderr, "All resources valid.") {
		t.Errorf("expected 'All resources valid.' after fixing the value, got:\n%s", stderr)
	}
}

// TestRunValidate_MissingKSPath_Fails pins the second half of the
// validation-gate contract: a Flux Kustomization whose spec.path is absent
// from the repository must fail validate with ExitCodeError instead of a
// silent skip that reports "All resources valid." while its resources
// never reach the checked set. Build and diff keep the lenient
// warn-and-continue behavior.
func TestRunValidate_MissingKSPath_Fails(t *testing.T) {
	repoRoot := t.TempDir()
	gitInit(t, repoRoot)

	// The KS points at a path that does not exist in the repo.
	clusterDir := filepath.Join(repoRoot, "cluster")
	writeHelper(t, clusterDir, "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: app
  namespace: flux-system
spec:
  interval: 5m
  path: ./does-not-exist
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
`)

	var runErr error
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:                  clusterDir,
			disableDefaultSchemas: true,
		})
	})

	exitErr, ok := runErr.(*DiffExitError)
	if !ok {
		t.Fatalf("expected *DiffExitError, got %v", runErr)
	}
	if exitErr.ExitCode != ExitCodeError {
		t.Errorf("exit code = %d, want %d (ExitCodeError)", exitErr.ExitCode, ExitCodeError)
	}
	if !strings.Contains(exitErr.Error(), "missing from the repository") {
		t.Errorf("error should report the missing path, got: %v", exitErr.Error())
	}
	if !strings.Contains(exitErr.Error(), "./does-not-exist") {
		t.Errorf("error should name the missing path, got: %v", exitErr.Error())
	}
	if !strings.Contains(stderr, "Warning: flux-system/app path ./does-not-exist not found locally") {
		t.Errorf("expected the per-KS warning in stderr, got:\n%s", stderr)
	}
	if strings.Contains(stderr, "All resources valid.") {
		t.Errorf("a missing spec.path must not report 'All resources valid.', got:\n%s", stderr)
	}
}

// TestRunValidate_SuspendedKS_MissingPathOK verifies suspended
// Kustomizations are exempt: Flux skips them, so a missing path is not an
// under-validation hole.
func TestRunValidate_SuspendedKS_MissingPathOK(t *testing.T) {
	repoRoot := t.TempDir()
	gitInit(t, repoRoot)

	clusterDir := filepath.Join(repoRoot, "cluster")
	writeHelper(t, clusterDir, "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: app
  namespace: flux-system
spec:
  interval: 5m
  path: ./does-not-exist
  prune: true
  suspend: true
  sourceRef:
    kind: GitRepository
    name: flux-system
`)

	var runErr error
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:                  clusterDir,
			disableDefaultSchemas: true,
		})
	})
	if runErr != nil {
		t.Fatalf("suspended KS with a missing path must not fail validate, got: %v", runErr)
	}
	if strings.Contains(stderr, "not found locally") {
		t.Errorf("suspended KS must not warn about its path, got:\n%s", stderr)
	}
}

// TestRunValidate_SkipKind wires --skip-kind end to end: the same invalid
// resource fails validation without the flag and is skipped with it.
func TestRunValidate_SkipKind(t *testing.T) {
	repoRoot := t.TempDir()
	gitInit(t, repoRoot)

	clusterDir := filepath.Join(repoRoot, "cluster")
	writeHelper(t, clusterDir, "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: app
  namespace: flux-system
spec:
  interval: 5m
  path: ./app
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
`)

	// A Widget resource missing the required spec.color.
	appDir := filepath.Join(repoRoot, "app")
	writeHelper(t, appDir, "kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - widget.yaml
`)
	writeHelper(t, appDir, "widget.yaml", `apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: my-widget
spec:
  size: large
`)

	schemaDir := filepath.Join(repoRoot, "schemas")
	if err := os.MkdirAll(schemaDir, 0755); err != nil {
		t.Fatalf("mkdir schema dir: %v", err)
	}
	writeHelper(t, schemaDir, "widget-test-v1.json", `{
	"type": "object",
	"properties": {
		"spec": {"type": "object", "required": ["color"], "properties": {
			"size": {"type": "string"},
			"color": {"type": "string"}
		}}
	}
}`)

	// Without --skip-kind the Widget fails validation (exit 3).
	var runErr error
	captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:                  clusterDir,
			SchemaDir:             schemaDir,
			disableDefaultSchemas: true,
		})
	})
	exitErr, ok := runErr.(*DiffExitError)
	if !ok {
		t.Fatalf("expected *DiffExitError, got %v", runErr)
	}
	if exitErr.ExitCode != ExitValidationFailed {
		t.Errorf("exit code = %d, want %d (ExitValidationFailed)", exitErr.ExitCode, ExitValidationFailed)
	}

	// With --skip-kind the Widget is skipped and the run passes.
	runErr = nil
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:                  clusterDir,
			SchemaDir:             schemaDir,
			SkipKinds:             []string{"Widget"},
			disableDefaultSchemas: true,
		})
	})
	if runErr != nil {
		t.Fatalf("expected success with --skip-kind, got: %v", runErr)
	}
	if !strings.Contains(stderr, "All resources valid.") {
		t.Errorf("expected 'All resources valid.' with --skip-kind, got:\n%s", stderr)
	}
}

// widgetCRDYAML is a minimal CRD for the Widget test resource.
const widgetCRDYAML = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.test.example.com
spec:
  group: test.example.com
  names:
    kind: Widget
    plural: widgets
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          required: [spec]
          properties:
            spec:
              type: object
              required: [color]
              properties:
                size:
                  type: string
                  enum: [small, medium, large]
                color:
                  type: string
`

func TestComposeSchemaLocations(t *testing.T) {
	testFlags := func() *ValidateFlags {
		return &ValidateFlags{testCacheBase: t.TempDir()}
	}

	t.Run("no schema dir yields only the prefetched registry copy", func(t *testing.T) {
		flags := testFlags()
		locations, _, err := composeSchemaLocations(flags, "")
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(flags.registryCacheDir(), "{{ .NormalizedKubernetesVersion }}-standalone{{ .StrictSuffix }}/{{ .ResourceKind }}{{ .KindSuffix }}.json")
		if len(locations) != 1 || locations[0] != want {
			t.Errorf("locations = %v, want [%s]", locations, want)
		}
	})

	t.Run("schema dir with CRD YAML adds local sources before the registry copy", func(t *testing.T) {
		dir := t.TempDir()
		writeHelper(t, dir, "widget-test-v1.json", `{"type": "object"}`)
		writeHelper(t, dir, "crd.yaml", widgetCRDYAML)

		flags := testFlags()
		locations, _, err := composeSchemaLocations(flags, dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(locations) != 3 {
			t.Fatalf("locations = %v, want 3", locations)
		}
		if locations[0] != filepath.Join(dir, "{{ .ResourceKind }}{{ .KindSuffix }}.json") {
			t.Errorf("first location = %q, want the schema dir template", locations[0])
		}
		if !strings.Contains(locations[1], "{{ .ResourceKind }}{{ .KindSuffix }}.json") {
			t.Errorf("second location = %q, want a converted-CRD template", locations[1])
		}
		if want := filepath.Join(flags.registryCacheDir(), "{{ .NormalizedKubernetesVersion }}-standalone{{ .StrictSuffix }}/{{ .ResourceKind }}{{ .KindSuffix }}.json"); locations[2] != want {
			t.Errorf("last location = %q, want the prefetched registry copy %q", locations[2], want)
		}
	})

	t.Run("schema dir without CRD YAML adds a single local source", func(t *testing.T) {
		dir := t.TempDir()
		writeHelper(t, dir, "widget-test-v1.json", `{"type": "object"}`)

		flags := testFlags()
		locations, _, err := composeSchemaLocations(flags, dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(locations) != 2 || locations[0] != filepath.Join(dir, "{{ .ResourceKind }}{{ .KindSuffix }}.json") {
			t.Errorf("locations = %v, want [dir template, registry copy]", locations)
		}
	})

	t.Run("disabling the registry yields an empty non-nil list", func(t *testing.T) {
		locations, _, err := composeSchemaLocations(&ValidateFlags{disableDefaultSchemas: true}, "")
		if err != nil {
			t.Fatal(err)
		}
		if locations == nil {
			t.Fatal("locations = nil, want non-nil: an empty list must not trigger validate.New's default fallback")
		}
		if len(locations) != 0 {
			t.Errorf("locations = %v, want none", locations)
		}
	})
}

func TestComposeSchemaLocations_KubernetesJSONSchemaCheckout(t *testing.T) {
	t.Run("checkout at the schema dir root", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "v1.36.1-standalone"), 0755); err != nil {
			t.Fatal(err)
		}
		writeHelper(t, dir, "v1.36.1-standalone/deployment-apps-v1.json", `{"type": "object"}`)

		flags := &ValidateFlags{testCacheBase: t.TempDir()}
		locations, _, err := composeSchemaLocations(flags, dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(locations) != 3 {
			t.Fatalf("locations = %v, want 3", locations)
		}
		want := filepath.Join(dir, "{{ .NormalizedKubernetesVersion }}-standalone{{ .StrictSuffix }}/{{ .ResourceKind }}{{ .KindSuffix }}.json")
		if locations[1] != want {
			t.Errorf("checkout location = %q,\nwant %q", locations[1], want)
		}
		if registry := filepath.Join(flags.registryCacheDir(), "{{ .NormalizedKubernetesVersion }}-standalone{{ .StrictSuffix }}/{{ .ResourceKind }}{{ .KindSuffix }}.json"); locations[2] != registry {
			t.Errorf("last location = %q, want the prefetched registry copy", locations[2])
		}
	})

	t.Run("checkout one level under the schema dir", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "k8s-schemas", "v1.36.1-standalone-strict"), 0755); err != nil {
			t.Fatal(err)
		}
		writeHelper(t, dir, "k8s-schemas/v1.36.1-standalone-strict/configmap-v1.json", `{"type": "object"}`)

		locations, _, err := composeSchemaLocations(&ValidateFlags{testCacheBase: t.TempDir()}, dir)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(dir, "k8s-schemas", "{{ .NormalizedKubernetesVersion }}-standalone{{ .StrictSuffix }}/{{ .ResourceKind }}{{ .KindSuffix }}.json")
		if len(locations) != 3 || locations[1] != want {
			t.Errorf("locations = %v,\nwant the nested checkout root %q", locations, want)
		}
	})

	t.Run("deeper nesting and plain files are not detected", func(t *testing.T) {
		dir := t.TempDir()
		// Two levels down: out of scope by design.
		if err := os.MkdirAll(filepath.Join(dir, "a", "b", "v1.36.1-standalone"), 0755); err != nil {
			t.Fatal(err)
		}
		writeHelper(t, dir, "a/b/v1.36.1-standalone/configmap-v1.json", `{"type": "object"}`)
		// A file that merely matches the pattern.
		writeHelper(t, dir, "notes-standalone", "not a directory")

		locations, _, err := composeSchemaLocations(&ValidateFlags{testCacheBase: t.TempDir()}, dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(locations) != 2 {
			t.Errorf("locations = %v, want 2 (root template + registry copy only)", locations)
		}
	})
}

func TestComposeSchemaLocations_CRDCacheAndErrors(t *testing.T) {
	t.Run("converted CRDs persist in the cache dir", func(t *testing.T) {
		dir := t.TempDir()
		writeHelper(t, dir, "crd.yaml", widgetCRDYAML)

		flags := &ValidateFlags{disableDefaultSchemas: true, testCacheBase: t.TempDir()}
		locations, _, err := composeSchemaLocations(flags, dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(locations) != 2 {
			t.Fatalf("locations = %v, want 2 (schema dir, converted CRDs)", locations)
		}

		crdDir := filepath.Dir(locations[1])
		if !strings.HasPrefix(crdDir, filepath.Join(flags.cacheBase(), "crd-schemas")) {
			t.Errorf("converted CRDs should live under the cache base, got %s", crdDir)
		}
		if _, statErr := os.Stat(filepath.Join(crdDir, "widget-test-v1.json")); statErr != nil {
			t.Errorf("converted schema should exist in the persistent cache: %v", statErr)
		}

		// A second compose reuses the same cache dir.
		locations2, _, err := composeSchemaLocations(flags, dir)
		if err != nil {
			t.Fatal(err)
		}
		if locations2[1] != locations[1] {
			t.Errorf("cache dir should be stable across runs: %q vs %q", locations[1], locations2[1])
		}
	})

	t.Run("nonexistent schema dir yields an error", func(t *testing.T) {
		_, _, err := composeSchemaLocations(&ValidateFlags{testCacheBase: t.TempDir()}, "/nonexistent/crd-dir-$$")
		if err == nil {
			t.Error("expected an error for a nonexistent --schema-dir")
		}
	})
}

// --- Prefetch / version validation (follow-ups) ---

// TestRunValidate_BadKubernetesVersion pins the fail-fast contract for a
// malformed version: "1.36" (no patch) would 404 every default-registry
// schema, silently skip all native kinds and still report success — so it
// must fail with ExitCodeError before the build instead.
func TestRunValidate_BadKubernetesVersion(t *testing.T) {
	for _, version := range []string{"1.36", "v1.36", "latest"} {
		var runErr error
		captureStderr(func() {
			runErr = runValidate(context.Background(), &ValidateFlags{
				KubernetesVersion:     version,
				disableDefaultSchemas: true,
			})
		})
		exitErr, ok := runErr.(*DiffExitError)
		if !ok {
			t.Fatalf("version %q: expected *DiffExitError, got %v", version, runErr)
		}
		if exitErr.ExitCode != ExitCodeError {
			t.Errorf("version %q: exit code = %d, want %d", version, exitErr.ExitCode, ExitCodeError)
		}
		if !strings.Contains(exitErr.Error(), "--kubernetes-version") {
			t.Errorf("version %q: error should name the flag, got: %v", version, exitErr.Error())
		}
	}
}

// validateFixture is a minimal repo: one Flux Kustomization pointing at a
// native kustomization with a single ConfigMap.
type validateFixture struct {
	repoRoot, clusterDir string
}

func newValidateFixture(t *testing.T, configmapValue string) *validateFixture {
	t.Helper()
	repoRoot := t.TempDir()
	gitInit(t, repoRoot)

	clusterDir := filepath.Join(repoRoot, "cluster")
	writeHelper(t, clusterDir, "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: app
  namespace: flux-system
spec:
  interval: 5m
  path: ./app
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
`)

	appDir := filepath.Join(repoRoot, "app")
	writeHelper(t, appDir, "kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - configmap.yaml
`)
	writeHelper(t, appDir, "configmap.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: app-config\ndata:\n  key: "+configmapValue+"\n")

	return &validateFixture{repoRoot: repoRoot, clusterDir: clusterDir}
}

// schemaServer is a stub default-registry: known paths serve their body,
// everything else 404s; per-path request counts are recorded.
type schemaServer struct {
	url    string
	mu     sync.Mutex
	counts map[string]int
}

func newSchemaServer(t *testing.T, schemas map[string]string) *schemaServer {
	t.Helper()
	stub := &schemaServer{counts: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.counts[r.URL.Path]++
		stub.mu.Unlock()
		body, ok := schemas[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(server.Close)
	stub.url = server.URL
	return stub
}

func (s *schemaServer) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[path]
}

func (s *schemaServer) totalCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, c := range s.counts {
		total += c
	}
	return total
}

// stringDataSchema rejects non-string ConfigMap data values.
const stringDataSchema = `{
	"type": "object",
	"properties": {
		"data": {"type": "object", "additionalProperties": {"type": "string"}}
	}
}`

// TestRunValidate_Prefetch wires the bounded download path end to end:
// schemas are prefetched from the registry into the local cache and
// validation reads them from there — the invalid ConfigMap fails against
// the prefetched schema, proving the schema came through this pipeline.
// Flux CRs are exempt: the registry never carries them, so they are not
// even requested.
func TestRunValidate_Prefetch(t *testing.T) {
	fixture := newValidateFixture(t, "42") // integer data value

	stub := newSchemaServer(t, map[string]string{
		"/v1.36.1-standalone/configmap-v1.json":               stringDataSchema,
		"/v1.36.1-standalone/kustomization-kustomize-v1.json": `{"type": "object"}`,
	})

	cacheBase := t.TempDir()
	var runErr error
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:            fixture.clusterDir,
			testRegistryURL: stub.url,
			testCacheBase:   cacheBase,
		})
	})

	exitErr, ok := runErr.(*DiffExitError)
	if !ok {
		t.Fatalf("expected *DiffExitError, got %v", runErr)
	}
	if exitErr.ExitCode != ExitValidationFailed {
		t.Errorf("exit code = %d, want %d (ExitValidationFailed)", exitErr.ExitCode, ExitValidationFailed)
	}
	if !strings.Contains(stderr, "ConfigMap app-config") {
		t.Errorf("expected the ConfigMap failure against the prefetched schema, got:\n%s", stderr)
	}
	if strings.Contains(stderr, "Warning: the default registry has no schema") {
		t.Errorf("a partially-covered registry must not trigger the mass-skip warning:\n%s", stderr)
	}
	if got := stub.count("/v1.36.1-standalone/kustomization-kustomize-v1.json"); got != 0 {
		t.Errorf("Flux CRs are never in the registry, must not be requested, got %d requests", got)
	}

	// The ConfigMap schema lives in the local cache, kubernetes-json-schema
	// layout; the Flux Kustomization schema must not be there.
	cmPath := filepath.Join(cacheBase, "schemas", "registry", "v1.36.1-standalone", "configmap-v1.json")
	if _, err := os.Stat(cmPath); err != nil {
		t.Errorf("expected prefetched schema %s: %v", cmPath, err)
	}
	ksPath := filepath.Join(cacheBase, "schemas", "registry", "v1.36.1-standalone", "kustomization-kustomize-v1.json")
	if _, err := os.Stat(ksPath); !os.IsNotExist(err) {
		t.Errorf("a Flux CR schema must not be prefetched, stat err: %v", err)
	}

	// A second run validates from the cache without new registry requests.
	before := stub.count("/v1.36.1-standalone/configmap-v1.json")
	stderr = captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:            fixture.clusterDir,
			testRegistryURL: stub.url,
			testCacheBase:   cacheBase,
		})
	})
	if runErr == nil {
		t.Error("the second run must fail the same way")
	}
	if got := stub.count("/v1.36.1-standalone/configmap-v1.json"); got != before {
		t.Errorf("cached schema was re-requested: %d → %d requests", before, got)
	}
}

// TestRunValidate_Prefetch_MassSkipWarning pins the typo-version safety
// net: when the registry has no schema for any of the uncovered kinds
// (exactly what a published-but-wrong --kubernetes-version looks like),
// the run still skips them but says so loudly instead of a silent green.
func TestRunValidate_Prefetch_MassSkipWarning(t *testing.T) {
	fixture := newValidateFixture(t, "value")
	stub := newSchemaServer(t, nil) // 404 everything

	var runErr error
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:              fixture.clusterDir,
			KubernetesVersion: "1.35.0", // valid semver, absent from the stub
			testRegistryURL:   stub.url,
			testCacheBase:     t.TempDir(),
		})
	})
	if runErr != nil {
		t.Fatalf("missing schemas must skip, not fail: %v", runErr)
	}
	if !strings.Contains(stderr, "All resources valid.") {
		t.Errorf("expected 'All resources valid.', got:\n%s", stderr)
	}
	// The Flux Kustomization CR is exempt from the mass-skip condition, so
	// exactly one kind (the ConfigMap) counts.
	if !strings.Contains(stderr, "the default registry has no schema for any of the 1 kind(s)") {
		t.Errorf("expected the mass-skip warning for 1 kind, got:\n%s", stderr)
	}
	if !strings.Contains(stderr, "1.35.0") {
		t.Errorf("the warning should name the requested version, got:\n%s", stderr)
	}
}

// TestRunValidate_Prefetch_FailClosed pins the fail-closed contract for
// registry failures worse than 404: a persistent 500 must fail the run
// with ExitCodeError, never silently skip validation.
func TestRunValidate_Prefetch_FailClosed(t *testing.T) {
	fixture := newValidateFixture(t, "value")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	var runErr error
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:            fixture.clusterDir,
			testRegistryURL: server.URL,
			testCacheBase:   t.TempDir(),
		})
	})
	exitErr, ok := runErr.(*DiffExitError)
	if !ok {
		t.Fatalf("expected *DiffExitError, got %v", runErr)
	}
	if exitErr.ExitCode != ExitCodeError {
		t.Errorf("exit code = %d, want %d (ExitCodeError)", exitErr.ExitCode, ExitCodeError)
	}
	if !strings.Contains(exitErr.Error(), "downloading Kubernetes schemas") {
		t.Errorf("error should name the schema download, got: %v", exitErr.Error())
	}
	if strings.Contains(stderr, "All resources valid.") {
		t.Errorf("a failed download must not report 'All resources valid.', got:\n%s", stderr)
	}
}

// TestRunValidate_BrokenCRDInSchemaDirFails pins the validation-gate
// contract end to end: a malformed CRD YAML in --schema-dir fails the run
// (exit 2) instead of warning and validating with silently missing CRD
// schemas — symmetric with a broken JSON schema file.
func TestRunValidate_BrokenCRDInSchemaDirFails(t *testing.T) {
	fixture := newValidateFixture(t, "value")

	schemaDir := filepath.Join(fixture.repoRoot, "schemas")
	if err := os.MkdirAll(schemaDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeHelper(t, schemaDir, "broken-crd.yaml", "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nspec:\n\tgroup: broken\n")

	var runErr error
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:                  fixture.clusterDir,
			SchemaDir:             schemaDir,
			disableDefaultSchemas: true,
			testCacheBase:         t.TempDir(),
		})
	})
	exitErr, ok := runErr.(*DiffExitError)
	if !ok {
		t.Fatalf("expected *DiffExitError, got %v", runErr)
	}
	if exitErr.ExitCode != ExitCodeError {
		t.Errorf("exit code = %d, want %d (ExitCodeError)", exitErr.ExitCode, ExitCodeError)
	}
	if !strings.Contains(exitErr.Error(), "could not decode") {
		t.Errorf("error should name the broken CRD file, got: %v", exitErr.Error())
	}
	if strings.Contains(stderr, "All resources valid.") {
		t.Errorf("a broken schema source must not report 'All resources valid.', got:\n%s", stderr)
	}
}

// TestRunValidate_SchemaDownloadTimeout pins the --schema-download-timeout
// contract: negative durations are rejected up front, 0 means no limit, and
// a small timeout turns a hung registry into a fast fail-closed exit
// instead of an endless wait.
func TestRunValidate_SchemaDownloadTimeout(t *testing.T) {
	t.Run("negative duration is rejected", func(t *testing.T) {
		var runErr error
		captureStderr(func() {
			runErr = runValidate(context.Background(), &ValidateFlags{
				SchemaDownloadTimeout: -1,
				disableDefaultSchemas: true,
			})
		})
		exitErr, ok := runErr.(*DiffExitError)
		if !ok {
			t.Fatalf("expected *DiffExitError, got %v", runErr)
		}
		if exitErr.ExitCode != ExitCodeError {
			t.Errorf("exit code = %d, want %d", exitErr.ExitCode, ExitCodeError)
		}
		if !strings.Contains(exitErr.Error(), "--schema-download-timeout") {
			t.Errorf("error should name the flag, got: %v", exitErr.Error())
		}
	})

	t.Run("a small timeout fails fast on a hung registry", func(t *testing.T) {
		fixture := newValidateFixture(t, "value")
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-release
		}))
		t.Cleanup(func() { close(release); server.Close() })

		var runErr error
		stderr := captureStderr(func() {
			runErr = runValidate(context.Background(), &ValidateFlags{
				Path:                  fixture.clusterDir,
				SchemaDownloadTimeout: 10 * time.Millisecond,
				testRegistryURL:       server.URL,
				testCacheBase:         t.TempDir(),
			})
		})
		exitErr, ok := runErr.(*DiffExitError)
		if !ok {
			t.Fatalf("expected *DiffExitError, got %v", runErr)
		}
		if exitErr.ExitCode != ExitCodeError {
			t.Errorf("exit code = %d, want %d (ExitCodeError)", exitErr.ExitCode, ExitCodeError)
		}
		if !strings.Contains(exitErr.Error(), "downloading Kubernetes schemas") {
			t.Errorf("error should name the schema download, got: %v", exitErr.Error())
		}
		if strings.Contains(stderr, "All resources valid.") {
			t.Errorf("a hung registry must not report 'All resources valid.', got:\n%s", stderr)
		}
	})
}

// TestRunValidate_Prefetch_LocalCoverageSkipsFetch proves the prefetch only
// asks the registry for kinds without a local schema: with ConfigMap
// covered by --schema-dir and the Flux Kustomization exempt as a Flux CR,
// only the Deployment is downloaded.
func TestRunValidate_Prefetch_LocalCoverageSkipsFetch(t *testing.T) {
	fixture := newValidateFixture(t, "value")

	// A second native resource the schema dir does not cover.
	appDir := filepath.Join(fixture.repoRoot, "app")
	writeHelper(t, appDir, "deployment.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
`)
	writeHelper(t, appDir, "kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - configmap.yaml
  - deployment.yaml
`)

	schemaDir := filepath.Join(fixture.repoRoot, "schemas")
	if err := os.MkdirAll(schemaDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeHelper(t, schemaDir, "configmap-v1.json", `{"type": "object"}`)

	stub := newSchemaServer(t, map[string]string{
		"/v1.36.1-standalone/deployment-apps-v1.json": `{"type": "object"}`,
	})

	var runErr error
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:            fixture.clusterDir,
			SchemaDir:       schemaDir,
			testRegistryURL: stub.url,
			testCacheBase:   t.TempDir(),
		})
	})
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	if !strings.Contains(stderr, "All resources valid.") {
		t.Errorf("expected 'All resources valid.', got:\n%s", stderr)
	}
	if got := stub.count("/v1.36.1-standalone/configmap-v1.json"); got != 0 {
		t.Errorf("the locally covered ConfigMap must not be requested, got %d requests", got)
	}
	if got := stub.count("/v1.36.1-standalone/deployment-apps-v1.json"); got != 1 {
		t.Errorf("the uncovered Deployment must be fetched exactly once, got %d requests", got)
	}
	if got := stub.count("/v1.36.1-standalone/kustomization-kustomize-v1.json"); got != 0 {
		t.Errorf("the Flux Kustomization is never in the registry, got %d requests", got)
	}
}

// TestRunValidate_Prefetch_FluxOnlyUncovered_NoWarning pins the review
// regression: when every custom kind is covered by --schema-dir and the
// only uncovered kinds are Flux CRs (never present in the registry), the
// mass-skip warning must stay silent — a missing kustomization schema is
// not evidence of a wrong --kubernetes-version.
func TestRunValidate_Prefetch_FluxOnlyUncovered_NoWarning(t *testing.T) {
	fixture := newValidateFixture(t, "value")

	schemaDir := filepath.Join(fixture.repoRoot, "schemas")
	if err := os.MkdirAll(schemaDir, 0755); err != nil {
		t.Fatal(err)
	}
	writeHelper(t, schemaDir, "configmap-v1.json", `{"type": "object"}`)

	stub := newSchemaServer(t, nil) // 404 everything, should not be asked

	var runErr error
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:              fixture.clusterDir,
			SchemaDir:         schemaDir,
			KubernetesVersion: "1.35.0",
			testRegistryURL:   stub.url,
			testCacheBase:     t.TempDir(),
		})
	})
	if runErr != nil {
		t.Fatalf("unexpected error: %v", runErr)
	}
	if !strings.Contains(stderr, "All resources valid.") {
		t.Errorf("expected 'All resources valid.', got:\n%s", stderr)
	}
	if strings.Contains(stderr, "Warning: the default registry has no schema") {
		t.Errorf("Flux-only uncovered kinds must not trip the mass-skip warning:\n%s", stderr)
	}
	if got := stub.totalCount(); got != 0 {
		t.Errorf("nothing is fetchable from the registry, want 0 requests, got %d", got)
	}
}
