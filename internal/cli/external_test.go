package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/skeema/knownhosts"
	"github.com/spf13/cobra"
	gossh "golang.org/x/crypto/ssh"

	"github.com/banschikovde/fluxview/internal/gitsource"
	"github.com/banschikovde/fluxview/internal/gitsource/gitsourcetest"
)

// hostOf extracts the bare hostname of a test server URL, for the
// FLUXVIEW_GIT_CREDENTIAL_HOSTS allowlist.
func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing %q: %v", raw, err)
	}
	return u.Hostname()
}

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

// TestBuildKS_InRepoGitSourceCacheStaysInvisible reproduces the CI layout
// that polluted builds after the external-source feature: the fluxview
// git-source cache lives INSIDE the fleet checkout (.cache-fluxview/...),
// and the tree walks — resource parsing with the repoRoot fallback, the
// remote-ref prefetch scan, loose-file reads — descended into the cached
// upstream clones, warning about their Go-template YAML, their
// mis-shaped ConfigMaps and their kustomizations' remote refs. Walks must
// never cross a repository boundary, wherever the cache is placed.
func TestBuildKS_InRepoGitSourceCacheStaysInvisible(t *testing.T) {
	f := newExternalFixture(t)

	// Move the GitRepository out of the cluster path (a sources/ dir at the
	// fleet root): resolving it now requires the repoRoot-fallback walk —
	// the walk that used to descend into the in-repo cache.
	if err := os.Remove(filepath.Join(f.clusterDir, "gitrepository.yaml")); err != nil {
		t.Fatalf("remove cluster gitrepository.yaml: %v", err)
	}
	writeHelper(t, filepath.Join(f.fleetDir, "sources"), "gitrepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: kyverno
  namespace: flux-system
spec:
  url: `+f.upstreamURL()+`
  ref:
    tag: v1.0.0
`)

	// CI layout: the git-source cache under the checkout. A leftover clone
	// under a foreign content key mirrors any previously fetched upstream:
	// a .git entry (repository boundary) plus the kinds of files that made
	// the pre-fix runs warn.
	clone := filepath.Join(f.fleetDir, ".cache-fluxview", "git-sources", "data", "deadbeef")
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0755); err != nil {
		t.Fatalf("mkdir clone .git: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(clone, "cmd", "cli", "templates"), 0755); err != nil {
		t.Fatalf("mkdir clone templates: %v", err)
	}
	// Unparseable Go-template YAML ("YAML parse error" pre-fix). A bare
	// "{{ ... }}" line parses as a flow mapping — the block form is what
	// actually breaks the decoder, as the real kyverno templates do.
	if err := os.WriteFile(filepath.Join(clone, ".krew.yaml"),
		[]byte("metadata:\n  name: krew\n{{- if .Values.enabled }}\n  annotations:\n{{- end }}\n"), 0644); err != nil {
		t.Fatalf("write .krew.yaml: %v", err)
	}
	// A ConfigMap whose data values are maps ("could not parse ConfigMap
	// document" pre-fix).
	if err := os.WriteFile(filepath.Join(clone, "cmd", "cli", "templates", "metrics-config.yaml"),
		[]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: metrics-config\ndata:\n  o:\n    app: kyverno\n"), 0644); err != nil {
		t.Fatalf("write metrics-config.yaml: %v", err)
	}
	// A kustomization with a remote ref ("could not download remote
	// resource" pre-fix); the bogus port fails fast if ever attempted.
	if err := os.MkdirAll(filepath.Join(clone, "scripts", "config", "kwok"), 0755); err != nil {
		t.Fatalf("mkdir kwok dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(clone, "scripts", "config", "kwok", "kustomization.yaml"),
		[]byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - http://127.0.0.1:1/kwok?ref=v0.2.0\n"), 0644); err != nil {
		t.Fatalf("write kwok kustomization: %v", err)
	}

	flags := buildFlagsFor(f)
	flags.GitSourceCacheDir = filepath.Join(f.fleetDir, ".cache-fluxview", "git-sources")

	var runErr error
	var stderr string
	stdout := captureStdout(func() {
		stderr = captureStderr(func() {
			runErr = runBuild(context.Background(), []string{"ks"}, flags)
		})
	})
	if runErr != nil {
		t.Fatalf("build ks with an in-repo git-source cache failed: %v", runErr)
	}
	for _, unwanted := range []string{
		"YAML parse error",
		"could not parse ConfigMap",
		"could not download remote resource",
	} {
		if strings.Contains(stderr, unwanted) {
			t.Errorf("the in-repo cached clone leaked into the build (%q):\n%s", unwanted, stderr)
		}
	}
	// The external source itself must still be fetched (into the same
	// in-repo cache) and built.
	if !strings.Contains(stdout, "policies.kyverno.io") {
		t.Errorf("external CRD missing from the build output:\n%s", stdout)
	}
}

// clearGitAuthEnv neutralizes the git-source auth environment so e2e
// tests never see the developer machine's credentials.
func clearGitAuthEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{
		"FLUXVIEW_GIT_SSH_KEY", "FLUXVIEW_GIT_SSH_PASSPHRASE",
		"FLUXVIEW_GIT_SSH_KNOWN_HOSTS", "FLUXVIEW_GIT_SSH_ACCEPT_NEW",
		"FLUXVIEW_GIT_USERNAME", "FLUXVIEW_GIT_PASSWORD",
		"FLUXVIEW_GIT_TOKEN", "FLUXVIEW_GIT_CREDENTIAL_HOSTS",
		"SSH_AUTH_SOCK",
	} {
		t.Setenv(v, "")
	}
	t.Setenv("HOME", t.TempDir())
}

// defaultBranchOf names the upstream's default branch (git CLI fixtures
// may default to either master or main depending on the machine).
func defaultBranchOf(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(gitOutput(t, dir, "symbolic-ref", "--short", "HEAD"))
}

// TestBuildKS_ExternalHTTPSBasicAuth pins the https e2e of ТЗ-1: a
// private upstream behind basic auth builds cleanly once the env pair is
// set, and the external CRD lands in the output.
func TestBuildKS_ExternalHTTPSBasicAuth(t *testing.T) {
	clearGitAuthEnv(t)
	f := newExternalFixture(t)

	bare := gitsourcetest.BareClone(t, f.upstreamDir, "upstream")
	base := gitsourcetest.HTTPSBasicAuth(t, filepath.Dir(bare), "fluxview", "s3cret")
	writeHelper(t, f.clusterDir, "gitrepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: kyverno
  namespace: flux-system
spec:
  url: `+base+`/upstream.git
  ref:
    tag: v1.0.0
`)
	t.Setenv("FLUXVIEW_GIT_USERNAME", "fluxview")
	t.Setenv("FLUXVIEW_GIT_PASSWORD", "s3cret")
	t.Setenv("FLUXVIEW_GIT_CREDENTIAL_HOSTS", hostOf(t, base))

	var runErr error
	stdout := captureStdout(func() {
		_ = captureStderr(func() {
			runErr = runBuild(context.Background(), []string{"ks"}, buildFlagsFor(f))
		})
	})
	if runErr != nil {
		t.Fatalf("build ks against a basic-auth upstream failed: %v", runErr)
	}
	if !strings.Contains(stdout, "policies.kyverno.io") {
		t.Errorf("external CRD missing from the build output:\n%s", stdout)
	}
}

// TestRunValidate_ExternalHTTPSCredsWithheld_Fails pins the H-1 fail-closed
// behavior: env credentials set but the host outside
// FLUXVIEW_GIT_CREDENTIAL_HOSTS must never reach the upstream — validate
// fails with the allowlist remedy in the message.
func TestRunValidate_ExternalHTTPSCredsWithheld_Fails(t *testing.T) {
	clearGitAuthEnv(t)
	f := newExternalFixture(t)

	bare := gitsourcetest.BareClone(t, f.upstreamDir, "upstream")
	base := gitsourcetest.HTTPSBasicAuth(t, filepath.Dir(bare), "fluxview", "s3cret")
	writeHelper(t, f.clusterDir, "gitrepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: kyverno
  namespace: flux-system
spec:
  url: `+base+`/upstream.git
  ref:
    tag: v1.0.0
`)
	t.Setenv("FLUXVIEW_GIT_USERNAME", "fluxview")
	t.Setenv("FLUXVIEW_GIT_PASSWORD", "s3cret")
	t.Setenv("FLUXVIEW_GIT_CREDENTIAL_HOSTS", "github.com")

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
	for _, want := range []string{
		"failed to fetch their external source",
		"private repository or bad credentials",
		"FLUXVIEW_GIT_CREDENTIAL_HOSTS",
	} {
		if !strings.Contains(exitErr.Error(), want) {
			t.Errorf("error must contain %q, got: %v", want, exitErr.Error())
		}
	}
	if strings.Contains(exitErr.Error(), "s3cret") {
		t.Errorf("error must not contain the password: %v", exitErr)
	}
}

// TestRunValidate_ExternalHTTPSNoCreds_Fails pins the 401 case of ТЗ-1:
// without credentials the private upstream fails validate with exit 2
// and the distinct bad-credentials message.
func TestRunValidate_ExternalHTTPSNoCreds_Fails(t *testing.T) {
	clearGitAuthEnv(t)
	f := newExternalFixture(t)

	bare := gitsourcetest.BareClone(t, f.upstreamDir, "upstream")
	base := gitsourcetest.HTTPSBasicAuth(t, filepath.Dir(bare), "fluxview", "s3cret")
	writeHelper(t, f.clusterDir, "gitrepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: kyverno
  namespace: flux-system
spec:
  url: `+base+`/upstream.git
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
	for _, want := range []string{
		"failed to fetch their external source",
		"private repository or bad credentials",
	} {
		if !strings.Contains(exitErr.Error(), want) {
			t.Errorf("error must contain %q, got: %v", want, exitErr.Error())
		}
	}
}

// TestRunValidate_ExternalSSHSource_Passes pins the ssh e2e of ТЗ-1: a
// private upstream over ssh (client key from env, strict known_hosts)
// validates green — including a floating branch ref, which resolves
// through the authenticated ls-remote.
func TestRunValidate_ExternalSSHSource_Passes(t *testing.T) {
	clearGitAuthEnv(t)
	f := newExternalFixture(t)

	bare := gitsourcetest.BareClone(t, f.upstreamDir, "upstream")

	keyPath, pub := newSSHClientKey(t)
	base, hostSigner := gitsourcetest.SSHGitServer(t, pub)

	khPath := filepath.Join(t.TempDir(), "known_hosts")
	kh, err := os.Create(khPath)
	if err != nil {
		t.Fatalf("creating known_hosts: %v", err)
	}
	if err := knownhosts.WriteKnownHost(kh, strings.TrimPrefix(base, "ssh://git@"), &net.TCPAddr{}, hostSigner.PublicKey()); err != nil {
		t.Fatalf("WriteKnownHost: %v", err)
	}
	kh.Close()

	t.Setenv("FLUXVIEW_GIT_SSH_KNOWN_HOSTS", khPath)
	t.Setenv("FLUXVIEW_GIT_SSH_KEY", keyPath)

	writeHelper(t, f.clusterDir, "gitrepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: kyverno
  namespace: flux-system
spec:
  url: `+base+bare+`
  ref:
    branch: `+defaultBranchOf(t, f.upstreamDir)+`
`)

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
		t.Fatalf("validate over ssh with a key must pass, got: %v\nstderr:\n%s", runErr, stderr)
	}
	if strings.Contains(stderr, "not found") {
		t.Errorf("no 'not found' warnings expected, got:\n%s", stderr)
	}
}

// newSSHClientKey generates a client key pair and writes the private key
// to a temp file; returns the path and the public key.
func newSSHClientKey(t *testing.T) (string, gossh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("NewSignerFromKey: %v", err)
	}
	block, err := gossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	path := filepath.Join(t.TempDir(), "id_e2e")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("writing client key: %v", err)
	}
	return path, signer.PublicKey()
}

// TestGitSourceAuthFlags_Wiring pins the flag plumbing: a flag the user
// actually passed is pushed into the env the auth resolver reads; flags
// left at their (env-derived) defaults never touch the env.
func TestGitSourceAuthFlags_Wiring(t *testing.T) {
	clearGitAuthEnv(t)

	newCmd := func(t *testing.T) (*cobra.Command, *string, *bool) {
		t.Helper()
		var knownHosts string
		var acceptNew bool
		cmd := &cobra.Command{
			RunE: func(cmd *cobra.Command, args []string) error {
				applyGitSourceAuthFlags(cmd.Flags(), knownHosts, acceptNew)
				return nil
			},
		}
		registerGitSourceAuthFlags(cmd, &knownHosts, &acceptNew)
		return cmd, &knownHosts, &acceptNew
	}

	t.Run("explicit flags are pushed to the env", func(t *testing.T) {
		cmd, _, _ := newCmd(t)
		cmd.SetArgs([]string{"--git-source-ssh-known-hosts", "/custom/kh", "--git-source-ssh-accept-new"})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
		if got := os.Getenv(gitsource.EnvSSHKnownHosts); got != "/custom/kh" {
			t.Errorf("env %s = %q, want /custom/kh", gitsource.EnvSSHKnownHosts, got)
		}
		if got := os.Getenv(gitsource.EnvSSHAcceptNew); got != "true" {
			t.Errorf("env %s = %q, want true", gitsource.EnvSSHAcceptNew, got)
		}
	})

	t.Run("unset flags leave the env alone", func(t *testing.T) {
		t.Setenv(gitsource.EnvSSHKnownHosts, "/from/env")
		cmd, _, _ := newCmd(t)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
		if got := os.Getenv(gitsource.EnvSSHKnownHosts); got != "/from/env" {
			t.Errorf("env must stay %q, got %q", "/from/env", got)
		}
	})

	t.Run("flag defaults come from the env", func(t *testing.T) {
		t.Setenv(gitsource.EnvSSHAcceptNew, "yes")
		cmd, _, acceptNew := newCmd(t)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("execute: %v", err)
		}
		if !*acceptNew {
			t.Error("accept-new flag default must pick up the env value")
		}
	})
}

// TestRunValidate_ExternalSSHAcceptNewFlag chains the whole policy path:
// --git-source-ssh-accept-new (pushed as an explicit flag) lets validate
// clone an ssh upstream whose host key is nowhere in known_hosts — TOFU
// through the real flag wiring, with no known_hosts file at all.
func TestRunValidate_ExternalSSHAcceptNewFlag(t *testing.T) {
	clearGitAuthEnv(t)
	f := newExternalFixture(t)
	bare := gitsourcetest.BareClone(t, f.upstreamDir, "upstream")

	keyPath, pub := newSSHClientKey(t)
	base, _ := gitsourcetest.SSHGitServer(t, pub)
	t.Setenv("FLUXVIEW_GIT_SSH_KEY", keyPath)

	// Simulate the cobra path: an explicitly passed flag lands in the env
	// before the pipeline runs.
	var knownHosts string
	var acceptNew bool
	flagCmd := &cobra.Command{
		RunE: func(cmd *cobra.Command, args []string) error {
			applyGitSourceAuthFlags(cmd.Flags(), knownHosts, acceptNew)
			return nil
		},
	}
	registerGitSourceAuthFlags(flagCmd, &knownHosts, &acceptNew)
	flagCmd.SetArgs([]string{"--git-source-ssh-accept-new"})
	if err := flagCmd.Execute(); err != nil {
		t.Fatalf("flag wiring: %v", err)
	}

	writeHelper(t, f.clusterDir, "gitrepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: kyverno
  namespace: flux-system
spec:
  url: `+base+bare+`
  ref:
    branch: `+defaultBranchOf(t, f.upstreamDir)+`
`)

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
		t.Fatalf("validate with --git-source-ssh-accept-new must pass without known_hosts, got: %v\nstderr:\n%s", runErr, stderr)
	}
}
