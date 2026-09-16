package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// externalFixture assembles the two repositories the external-source
// pipeline spans: a fleet ("cluster") repository with an origin remote, and
// an upstream repository whose config/crds subtree is the external source.
type externalFixture struct {
	t           *testing.T
	fleetDir    string
	clusterDir  string
	upstreamDir string
}

// newExternalFixture creates the fleet repo (origin remote, Flux
// Kustomization + external GitRepository manifests) and the upstream repo
// (config/crds with a kustomization and one manifest).
func newExternalFixture(t *testing.T) *externalFixture {
	t.Helper()

	upstreamDir := t.TempDir()
	gitRun(t, upstreamDir, "init", "-q")
	upstreamCommit(t, upstreamDir, "config/crds/kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - crd.yaml
`)
	upstreamCommit(t, upstreamDir, "config/crds/crd.yaml", `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: policies.kyverno.io
`)
	gitRun(t, upstreamDir, "tag", "v1.0.0")

	fleetDir := t.TempDir()
	gitRun(t, fleetDir, "init", "-q")
	gitRun(t, fleetDir, "remote", "add", "origin", "https://example.com/org/fleet.git")

	f := &externalFixture{
		t:           t,
		fleetDir:    fleetDir,
		clusterDir:  filepath.Join(fleetDir, "cluster"),
		upstreamDir: upstreamDir,
	}

	writeHelper(t, f.clusterDir, "gitrepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: kyverno
  namespace: flux-system
spec:
  url: `+f.upstreamURL()+`
  ref:
    tag: v1.0.0
`)
	writeHelper(t, f.clusterDir, "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: kyverno-crds
  namespace: flux-system
spec:
  path: ./config/crds
  sourceRef:
    kind: GitRepository
    name: kyverno
`)
	return f
}

func (f *externalFixture) upstreamURL() string {
	return "file://" + f.upstreamDir
}

// ksPath rewrites the Kustomization's spec.path in the fleet repo.
func (f *externalFixture) setKSPath(path string) {
	f.t.Helper()
	writeHelper(f.t, f.clusterDir, "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: kyverno-crds
  namespace: flux-system
spec:
  path: `+path+`
  sourceRef:
    kind: GitRepository
    name: kyverno
`)
}

// setUpstreamRef rewrites the GitRepository's ref in the fleet repo.
func (f *externalFixture) setUpstreamRef(ref string) {
	f.t.Helper()
	writeHelper(f.t, f.clusterDir, "gitrepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: kyverno
  namespace: flux-system
spec:
  url: `+f.upstreamURL()+`
`+ref)
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// upstreamCommit writes/overwrites one file in the upstream repo and commits.
func upstreamCommit(t *testing.T, dir, path, content string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "-c", "user.name=test", "-c", "user.email=test@test.com",
		"commit", "-q", "-m", "add "+path)
}

func buildFlagsFor(f *externalFixture) *BuildFlags {
	return &BuildFlags{
		Path:              f.clusterDir,
		GitSourceCacheDir: filepath.Join(f.t.TempDir(), "git-sources"),
		GitSourceCacheTTL: time.Hour,
	}
}

// TestBuildKS_ExternalGitRepositorySource is the core acceptance of the
// bug: a Kustomization whose path lives in an external GitRepository is
// fetched and built — the upstream CRD appears in the build output.
func TestBuildKS_ExternalGitRepositorySource(t *testing.T) {
	f := newExternalFixture(t)

	var runErr error
	stdout := captureStdout(func() {
		stderr := captureStderr(func() {
			runErr = runBuild(context.Background(), []string{"ks"}, buildFlagsFor(f))
		})
		// User-visible behavior must match local manifests exactly: the
		// only progress line is "Building ns/name"; no fetch notices.
		if !strings.Contains(stderr, "Building flux-system/kyverno-crds") {
			t.Errorf("expected the ordinary 'Building' progress line, got:\n%s", stderr)
		}
		if strings.Contains(stderr, "Fetching external source") {
			t.Errorf("external sources must be fetched silently, got:\n%s", stderr)
		}
		if strings.Contains(stderr, "not found locally") {
			t.Errorf("external path must not warn 'not found locally', got:\n%s", stderr)
		}
	})
	if runErr != nil {
		t.Fatalf("build ks with an external source failed: %v", runErr)
	}
	if !strings.Contains(stdout, "policies.kyverno.io") {
		t.Errorf("build output must include the external CRD, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "kind: Kustomization") {
		t.Errorf("build output must still include the KS resource itself, got:\n%s", stdout)
	}
}

// TestBuildKS_ExternalSource_ChainedUpstreams pins recursion: an external
// clone's build output holds another Flux Kustomization pointing at a
// second external GitRepository — both upstreams are fetched and built.
func TestBuildKS_ExternalSource_ChainedUpstreams(t *testing.T) {
	f := newExternalFixture(t)

	// Second upstream: referenced only from the first upstream's output.
	level2Dir := t.TempDir()
	gitRun(t, level2Dir, "init", "-q")
	upstreamCommit(t, level2Dir, "data/crds/kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - crd2.yaml
`)
	upstreamCommit(t, level2Dir, "data/crds/crd2.yaml", `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: reports.kyverno.io
`)
	gitRun(t, level2Dir, "tag", "v2.0.0")

	// The first upstream's kustomization also emits a Flux Kustomization
	// pointing at the second upstream, plus its GitRepository source.
	upstreamCommit(t, f.upstreamDir, "config/crds/ks-level2.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: level2
  namespace: flux-system
spec:
  url: file://`+level2Dir+`
  ref:
    tag: v2.0.0
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: level2-crds
  namespace: flux-system
spec:
  path: ./data/crds
  sourceRef:
    kind: GitRepository
    name: level2
`)
	// kustomize emits only listed resources: the chaining manifests must be
	// part of the build for discovery to see them.
	upstreamCommit(t, f.upstreamDir, "config/crds/kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - crd.yaml
  - ks-level2.yaml
`)
	// The pinned tag must include the chaining manifests.
	gitRun(t, f.upstreamDir, "tag", "-f", "v1.0.0")

	var runErr error
	var stderr string
	stdout := captureStdout(func() {
		stderr = captureStderr(func() {
			runErr = runBuild(context.Background(), []string{"ks"}, buildFlagsFor(f))
		})
	})
	if runErr != nil {
		t.Fatalf("build ks with chained external sources failed: %v\nstderr:\n%s", runErr, stderr)
	}
	for _, want := range []string{"policies.kyverno.io", "reports.kyverno.io"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("build output must include %s from the chained upstream, got:\n%s\nstderr:\n%s", want, stdout, stderr)
		}
	}
}

// TestBuildKS_ExternalFetchFailure_WarnAndSkip: build stays lenient — a
// warning names the source, the KS resource is included, no error.
func TestBuildKS_ExternalFetchFailure_WarnAndSkip(t *testing.T) {
	f := newExternalFixture(t)
	// Point the GitRepository at a nonexistent repository.
	writeHelper(t, f.clusterDir, "gitrepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: kyverno
  namespace: flux-system
spec:
  url: file://`+filepath.Join(t.TempDir(), "missing-upstream")+`
  ref:
    tag: v1.0.0
`)

	var runErr error
	stdout := captureStdout(func() {
		stderr := captureStderr(func() {
			runErr = runBuild(context.Background(), []string{"ks"}, buildFlagsFor(f))
		})
		if !strings.Contains(stderr, "fetching GitRepository flux-system/kyverno for flux-system/kyverno-crds failed") {
			t.Errorf("expected the fetch-failure warning, got:\n%s", stderr)
		}
	})
	if runErr != nil {
		t.Fatalf("build must not fail on an external fetch error, got: %v", runErr)
	}
	if strings.Contains(stdout, "policies.kyverno.io") {
		t.Errorf("external resources must be absent after the failed fetch, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "kyverno-crds") {
		t.Errorf("the KS resource itself must stay in the output, got:\n%s", stdout)
	}
}

// TestBuildKS_LocalSourceIsNotExternal: a GitRepository pointing at the
// fleet's own origin keeps the old semantics — the path resolves against
// the local repository only.
func TestBuildKS_LocalSourceIsNotExternal(t *testing.T) {
	f := newExternalFixture(t)
	writeHelper(t, f.clusterDir, "gitrepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: kyverno
  namespace: flux-system
spec:
  url: https://example.com/org/fleet.git
  ref:
    tag: v1.0.0
`)

	var runErr error
	stdout := captureStdout(func() {
		stderr := captureStderr(func() {
			runErr = runBuild(context.Background(), []string{"ks"}, buildFlagsFor(f))
		})
		if !strings.Contains(stderr, "path ./config/crds not found locally") {
			t.Errorf("same-repo source must warn 'not found locally', got:\n%s", stderr)
		}
	})
	if runErr != nil {
		t.Fatalf("build must stay lenient, got: %v", runErr)
	}
	if strings.Contains(stdout, "policies.kyverno.io") {
		t.Errorf("nothing external may be built for a local source, got:\n%s", stdout)
	}
}

// TestRunValidate_ExternalSource_Passes: with the upstream reachable the
// gate validates the tree including external resources — no more "path
// missing from the repository" failure on a valid tree.
func TestRunValidate_ExternalSource_Passes(t *testing.T) {
	f := newExternalFixture(t)

	var runErr error
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:                  f.clusterDir,
			disableDefaultSchemas: true,
			GitSourceCacheDir:     filepath.Join(t.TempDir(), "git-sources"),
			GitSourceCacheTTL:     time.Hour,
		})
	})
	if runErr != nil {
		t.Fatalf("validate with a buildable external source must pass, got: %v\nstderr:\n%s", runErr, stderr)
	}
	if strings.Contains(stderr, "not found") {
		t.Errorf("no 'not found' warnings expected, got:\n%s", stderr)
	}
}

// TestRunValidate_ExternalBrokenBuild_Fails: an error inside the external
// repository (a kustomization referencing a missing file) must fail the
// gate — external resources are checked, not skipped.
func TestRunValidate_ExternalBrokenBuild_Fails(t *testing.T) {
	f := newExternalFixture(t)
	upstreamCommit(t, f.upstreamDir, "config/crds/kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - does-not-exist.yaml
`)
	gitRun(t, f.upstreamDir, "tag", "-f", "v1.0.0")

	var runErr error
	_ = captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:                  f.clusterDir,
			disableDefaultSchemas: true,
			GitSourceCacheDir:     filepath.Join(t.TempDir(), "git-sources"),
			GitSourceCacheTTL:     time.Hour,
		})
	})
	exitErr, ok := runErr.(*DiffExitError)
	if !ok {
		t.Fatalf("expected *DiffExitError, got %v", runErr)
	}
	if exitErr.ExitCode != ExitCodeError {
		t.Errorf("exit code = %d, want %d", exitErr.ExitCode, ExitCodeError)
	}
	if !strings.Contains(exitErr.Error(), "cannot validate") {
		t.Errorf("error must fail the gate, got: %v", exitErr.Error())
	}
}

// TestRunValidate_ExternalFetchFailure_Fails: an unreachable upstream
// fails validate through report.fetchErrors — the gate does not report
// success on a partial check.
func TestRunValidate_ExternalFetchFailure_Fails(t *testing.T) {
	f := newExternalFixture(t)
	writeHelper(t, f.clusterDir, "gitrepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: kyverno
  namespace: flux-system
spec:
  url: file://`+filepath.Join(t.TempDir(), "missing-upstream")+`
  ref:
    tag: v1.0.0
`)

	var runErr error
	_ = captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:                  f.clusterDir,
			disableDefaultSchemas: true,
			GitSourceCacheDir:     filepath.Join(t.TempDir(), "git-sources"),
			GitSourceCacheTTL:     time.Hour,
		})
	})
	exitErr, ok := runErr.(*DiffExitError)
	if !ok {
		t.Fatalf("expected *DiffExitError, got %v", runErr)
	}
	if exitErr.ExitCode != ExitCodeError {
		t.Errorf("exit code = %d, want %d", exitErr.ExitCode, ExitCodeError)
	}
	if !strings.Contains(exitErr.Error(), "failed to fetch their external source") ||
		!strings.Contains(exitErr.Error(), "flux-system/kyverno-crds") {
		t.Errorf("error must name the fetch failure and the KS, got: %v", exitErr.Error())
	}
}

// TestRunValidate_ExternalPathMissingInClone_Fails: the upstream is
// fetched fine but does not contain the declared path — still a gate
// failure, never a silent skip.
func TestRunValidate_ExternalPathMissingInClone_Fails(t *testing.T) {
	f := newExternalFixture(t)
	f.setKSPath("./no-such-dir")

	var runErr error
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:                  f.clusterDir,
			disableDefaultSchemas: true,
			GitSourceCacheDir:     filepath.Join(t.TempDir(), "git-sources"),
			GitSourceCacheTTL:     time.Hour,
		})
	})
	exitErr, ok := runErr.(*DiffExitError)
	if !ok {
		t.Fatalf("expected *DiffExitError, got %v", runErr)
	}
	if !strings.Contains(exitErr.Error(), "missing from the repository") {
		t.Errorf("error must report the missing path, got: %v", exitErr.Error())
	}
	if !strings.Contains(stderr, "not found in external source") {
		t.Errorf("warning must name the external source, got:\n%s", stderr)
	}
}

// TestDiffKS_ExternalResourcesCompared: both diff sides build the external
// source through one shared clone cache; a moved tag between the cluster
// repo revisions surfaces as a real diff of the external resources.
func TestDiffKS_ExternalResourcesCompared(t *testing.T) {
	f := newExternalFixture(t)

	// Second upstream tag with different content.
	upstreamCommit(t, f.upstreamDir, "config/crds/crd.yaml", `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: policies.kyverno.io
  annotations:
    upstream-tag: v1.1.0
`)
	gitRun(t, f.upstreamDir, "tag", "v1.1.0")

	// Cluster repo history: first commit pins v1.0.0, HEAD pins v1.1.0.
	f.setUpstreamRef("  ref:\n    tag: v1.0.0\n")
	gitRun(t, f.fleetDir, "add", "-A")
	gitRun(t, f.fleetDir, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit", "-q", "-m", "pin v1.0.0")
	firstCommit := strings.TrimSpace(gitOutput(t, f.fleetDir, "rev-parse", "HEAD"))

	f.setUpstreamRef("  ref:\n    tag: v1.1.0\n")
	gitRun(t, f.fleetDir, "add", "-A")
	gitRun(t, f.fleetDir, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit", "-q", "-m", "pin v1.1.0")

	var runErr error
	stdout := captureStdout(func() {
		_ = captureStderr(func() {
			runErr = runDiff(context.Background(), []string{"ks"}, &DiffFlags{
				Path:              f.clusterDir,
				BranchOrig:        firstCommit,
				Color:             "never",
				GitSourceCacheDir: filepath.Join(t.TempDir(), "git-sources"),
				GitSourceCacheTTL: time.Hour,
			})
		})
	})
	// "differences found" is the diff command's success-with-changes exit —
	// here it is exactly the expected outcome.
	if runErr != nil && !strings.Contains(runErr.Error(), "differences found") {
		t.Fatalf("diff ks with external sources failed: %v", runErr)
	}
	if runErr == nil {
		t.Fatal("diff must report the external resource change as differences")
	}
	if !strings.Contains(stdout, "upstream-tag") || !strings.Contains(stdout, "v1.1.0") {
		t.Errorf("diff must show the external resource change between tags, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "tag: v1.1.0") && !strings.Contains(stdout, "v1.0.0") {
		t.Errorf("diff must show the GitRepository ref change, got:\n%s", stdout)
	}
}

// TestBuildKS_ExternalLooseFilesWithoutKustomization pins the blocker from
// the independent review: an external path that is a directory of loose
// YAML files with NO kustomization.yaml (the real kyverno config/crds
// shape) must contribute its files to the output — the loose-file walker
// used to scope reads to the local repository root and silently dropped
// every file of the clone.
func TestBuildKS_ExternalLooseFilesWithoutKustomization(t *testing.T) {
	upstreamDir := t.TempDir()
	gitRun(t, upstreamDir, "init", "-q")
	upstreamCommit(t, upstreamDir, "config/crds/crd-a.yaml", `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: policies.kyverno.io
`)
	upstreamCommit(t, upstreamDir, "config/crds/crd-b.yaml", `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: reports.kyverno.io
`)
	gitRun(t, upstreamDir, "tag", "v1.0.0")

	fleetDir := t.TempDir()
	gitRun(t, fleetDir, "init", "-q")
	gitRun(t, fleetDir, "remote", "add", "origin", "https://example.com/org/fleet.git")
	clusterDir := filepath.Join(fleetDir, "cluster")
	writeHelper(t, clusterDir, "gitrepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: kyverno
  namespace: flux-system
spec:
  url: file://`+upstreamDir+`
  ref:
    tag: v1.0.0
`)
	writeHelper(t, clusterDir, "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: kyverno-crds
  namespace: flux-system
spec:
  path: ./config/crds
  sourceRef:
    kind: GitRepository
    name: kyverno
`)

	flags := &BuildFlags{
		Path:              clusterDir,
		GitSourceCacheDir: filepath.Join(t.TempDir(), "git-sources"),
		GitSourceCacheTTL: time.Hour,
	}

	var runErr error
	stdout := captureStdout(func() {
		stderr := captureStderr(func() {
			runErr = runBuild(context.Background(), []string{"ks"}, flags)
		})
		if strings.Contains(stderr, "could not read") {
			t.Errorf("loose files of the clone must be readable, got:\n%s", stderr)
		}
	})
	if runErr != nil {
		t.Fatalf("build ks from a loose-file external source failed: %v", runErr)
	}
	for _, want := range []string{"policies.kyverno.io", "reports.kyverno.io"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("build output must include the loose external file %s, got:\n%s", want, stdout)
		}
	}
}

// TestBuildKS_ExternalSourceFirstOverLocalCopy pins Flux source-first
// semantics: when sourceRef names an external GitRepository, spec.path is
// resolved against the upstream clone even if a directory of the same name
// exists locally — the local copy is another repository's content.
func TestBuildKS_ExternalSourceFirstOverLocalCopy(t *testing.T) {
	f := newExternalFixture(t)

	// A decoy: the same path exists locally with different content.
	writeHelper(t, filepath.Join(f.fleetDir, "config", "crds"), "kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - crd.yaml
`)
	writeHelper(t, filepath.Join(f.fleetDir, "config", "crds"), "crd.yaml", `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: local-copy.decoy.io
`)

	var runErr error
	stdout := captureStdout(func() {
		_ = captureStderr(func() {
			runErr = runBuild(context.Background(), []string{"ks"}, buildFlagsFor(f))
		})
	})
	if runErr != nil {
		t.Fatalf("build ks failed: %v", runErr)
	}
	if !strings.Contains(stdout, "policies.kyverno.io") {
		t.Errorf("build output must come from the upstream clone, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "local-copy.decoy.io") {
		t.Errorf("a local same-name directory must not shadow the external source, got:\n%s", stdout)
	}
}

// TestRunValidate_ExternalInvalidSchema_Fails covers the full acceptance of
// the bug: a resource inside the external repository that violates its
// schema fails validate (exit 3), proving external content is validated,
// not merely fetched.
func TestRunValidate_ExternalInvalidSchema_Fails(t *testing.T) {
	upstreamDir := t.TempDir()
	gitRun(t, upstreamDir, "init", "-q")
	// A Widget missing the required spec.color, as loose files (no
	// kustomization.yaml) — the strictest combination.
	upstreamCommit(t, upstreamDir, "config/crds/widget.yaml", `apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: external-widget
spec:
  size: large
`)
	gitRun(t, upstreamDir, "tag", "v1.0.0")

	fleetDir := t.TempDir()
	gitRun(t, fleetDir, "init", "-q")
	gitRun(t, fleetDir, "remote", "add", "origin", "https://example.com/org/fleet.git")
	clusterDir := filepath.Join(fleetDir, "cluster")
	writeHelper(t, clusterDir, "gitrepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: kyverno
  namespace: flux-system
spec:
  url: file://`+upstreamDir+`
  ref:
    tag: v1.0.0
`)
	writeHelper(t, clusterDir, "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: kyverno-crds
  namespace: flux-system
spec:
  path: ./config/crds
  sourceRef:
    kind: GitRepository
    name: kyverno
`)

	schemaDir := filepath.Join(fleetDir, "schemas")
	if err := os.MkdirAll(schemaDir, 0o755); err != nil {
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

	var runErr error
	stderr := captureStderr(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:                  clusterDir,
			SchemaDir:             schemaDir,
			disableDefaultSchemas: true,
			GitSourceCacheDir:     filepath.Join(t.TempDir(), "git-sources"),
			GitSourceCacheTTL:     time.Hour,
		})
	})
	exitErr, ok := runErr.(*DiffExitError)
	if !ok {
		t.Fatalf("expected *DiffExitError, got %v", runErr)
	}
	if exitErr.ExitCode != ExitValidationFailed {
		t.Errorf("exit code = %d, want %d (ExitValidationFailed)", exitErr.ExitCode, ExitValidationFailed)
	}
	if !strings.Contains(stderr, "external-widget") {
		t.Errorf("the failure must name the external resource, got:\n%s", stderr)
	}
}

// TestBuildKS_ExternalTransformationsAndSubstitute verifies the full KS
// post-processing chain applies to external content exactly as to local:
// targetNamespace, JSON6902 patches and postBuild.substitute.
func TestBuildKS_ExternalTransformationsAndSubstitute(t *testing.T) {
	upDir := t.TempDir()
	gitRun(t, upDir, "init", "-q")
	upstreamCommit(t, upDir, "config/app/kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - cm.yaml
`)
	upstreamCommit(t, upDir, "config/app/cm.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: ext-cm
data:
  cluster: ${cluster_name}
`)
	gitRun(t, upDir, "tag", "v1.0.0")

	fleetDir := t.TempDir()
	gitRun(t, fleetDir, "init", "-q")
	gitRun(t, fleetDir, "remote", "add", "origin", "https://example.com/org/fleet.git")
	clusterDir := filepath.Join(fleetDir, "cluster")
	writeHelper(t, clusterDir, "gitrepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: upstream
  namespace: flux-system
spec:
  url: file://`+upDir+`
  ref:
    tag: v1.0.0
`)
	writeHelper(t, clusterDir, "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: external-app
  namespace: flux-system
spec:
  path: ./config/app
  sourceRef:
    kind: GitRepository
    name: upstream
  targetNamespace: patched-ns
  patches:
    - target:
        kind: ConfigMap
        name: ext-cm
      patch: |-
        - op: add
          path: /data/patched
          value: "yes"
  postBuild:
    substitute:
      cluster_name: prod42
`)

	var runErr error
	stdout := captureStdout(func() {
		_ = captureStderr(func() {
			runErr = runBuild(context.Background(), []string{"ks"}, &BuildFlags{
				Path:              clusterDir,
				GitSourceCacheDir: filepath.Join(t.TempDir(), "git-sources"),
				GitSourceCacheTTL: time.Hour,
			})
		})
	})
	if runErr != nil {
		t.Fatalf("build ks failed: %v", runErr)
	}
	if !strings.Contains(stdout, "kind: ConfigMap") || !strings.Contains(stdout, "ext-cm") {
		t.Fatalf("external ConfigMap missing from output:\n%s", stdout)
	}
	if !strings.Contains(stdout, "namespace: patched-ns") {
		t.Errorf("targetNamespace must apply to external content, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "patched: \"yes\"") {
		t.Errorf("JSON6902 patch must apply to external content, got:\n%s", stdout)
	}
	if !strings.Contains(stdout, "cluster: prod42") {
		t.Errorf("postBuild substitute must apply to external content, got:\n%s", stdout)
	}
	if strings.Contains(stdout, "${cluster_name}") {
		t.Errorf("unsubstituted variable leaked into output:\n%s", stdout)
	}
}

// TestBuildKS_ExternalChainHitsMaxDepth builds an 11-level chain of external
// upstreams: the recursion must stop at maxDepth (10) with a warning, not
// hang or silently succeed deeper.
func TestBuildKS_ExternalChainHitsMaxDepth(t *testing.T) {
	const depth = 11

	dirs := make([]string, depth)
	for i := range dirs {
		dirs[i] = t.TempDir()
		gitRun(t, dirs[i], "init", "-q")
	}
	// Deepest upstream holds the payload.
	upstreamCommit(t, dirs[depth-1], "config/crds/kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - crd.yaml
`)
	upstreamCommit(t, dirs[depth-1], "config/crds/crd.yaml", `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: deepest.kyverno.io
`)
	gitRun(t, dirs[depth-1], "tag", "v1.0.0")

	// Each level's output references the next upstream's Kustomization.
	for i := 0; i < depth-1; i++ {
		upstreamCommit(t, dirs[i], "config/crds/ks-next.yaml", fmt.Sprintf(`apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: level-%d
  namespace: flux-system
spec:
  url: file://%s
  ref:
    tag: v1.0.0
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: level-%d-crds
  namespace: flux-system
spec:
  path: ./config/crds
  sourceRef:
    kind: GitRepository
    name: level-%d
`, i+1, dirs[i+1], i+1, i+1))
		upstreamCommit(t, dirs[i], "config/crds/kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ks-next.yaml
`)
		gitRun(t, dirs[i], "tag", "v1.0.0")
	}

	fleetDir := t.TempDir()
	gitRun(t, fleetDir, "init", "-q")
	gitRun(t, fleetDir, "remote", "add", "origin", "https://example.com/org/fleet.git")
	clusterDir := filepath.Join(fleetDir, "cluster")
	writeHelper(t, clusterDir, "gitrepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: level-0
  namespace: flux-system
spec:
  url: file://`+dirs[0]+`
  ref:
    tag: v1.0.0
`)
	writeHelper(t, clusterDir, "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: chain
  namespace: flux-system
spec:
  path: ./config/crds
  sourceRef:
    kind: GitRepository
    name: level-0
`)

	var runErr error
	var stderr string
	stdout := captureStdout(func() {
		stderr = captureStderr(func() {
			runErr = runBuild(context.Background(), []string{"ks"}, &BuildFlags{
				Path:              clusterDir,
				GitSourceCacheDir: filepath.Join(t.TempDir(), "git-sources"),
				GitSourceCacheTTL: time.Hour,
			})
		})
	})
	if runErr != nil {
		t.Fatalf("build ks through the chain failed: %v", runErr)
	}
	if !strings.Contains(stderr, "max recursion depth (10) reached") {
		t.Errorf("expected the max-depth warning, got:\n%s", stderr)
	}
	if strings.Contains(stdout, "deepest.kyverno.io") {
		t.Errorf("the 11th level must not be reached (maxDepth=10), got its CRD in output:\n%s", stdout)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v in %s: %v", args, dir, err)
	}
	return string(out)
}
