package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/helm"
	"github.com/banschikovde/fluxview/internal/kubeversion"
)

// TestKubeVersionFlagRegistration: the flag exists on every kube-version
// consuming command with the static pipeline default — the env is never
// read at registration time, so no command shows env-derived values or
// diagnostics before its RunE.
func TestKubeVersionFlagRegistration(t *testing.T) {
	for name, cmd := range map[string]*cobra.Command{
		"build": newBuildCmd(), "diff": newDiffCmd(), "inventory": newInventoryCmd(), "validate": newValidateCmd(),
	} {
		flag := cmd.Flags().Lookup("kubernetes-version")
		if flag == nil {
			t.Errorf("%s must register --kubernetes-version", name)
			continue
		}
		if got := flag.DefValue; got != kubeversion.Default {
			t.Errorf("%s flag default = %s, want the static pipeline default %s (the env is resolved at run time)", name, got, kubeversion.Default)
		}
	}
}

// TestKubeVersionLadder: unit table for the shared flag > env > default
// ladder (identical for every command — the env grammar is strict, the
// command-side flags carry their own grammars).
func TestKubeVersionLadder(t *testing.T) {
	cmdWithFlag := func(value string, set bool) *cobra.Command {
		cmd := &cobra.Command{}
		v := kubeversion.Default
		cmd.Flags().StringVar(&v, "kubernetes-version", kubeversion.Default, "")
		if set {
			if err := cmd.Flags().Set("kubernetes-version", value); err != nil {
				t.Fatalf("set flag: %v", err)
			}
		}
		return cmd
	}

	tests := []struct {
		name      string
		flagValue string
		flagSet   bool
		pin       string
		want      string
	}{
		{name: "nothing set gives the static default", pin: "", flagValue: kubeversion.Default, want: kubeversion.Default},
		{name: "pin wins over the default", pin: "1.35.7", flagValue: kubeversion.Default, want: "1.35.7"},
		{name: "flag wins over the pin", pin: "1.35.7", flagValue: "1.34.0", flagSet: true, want: "1.34.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := cmdWithFlag(tt.flagValue, tt.flagSet)
			if got := effectiveKubeVersion(cmd, tt.flagValue, tt.pin); got != tt.want {
				t.Errorf("effectiveKubeVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestKubeVersionEnvFailsEveryCommand: the strict env grammar is uniform —
// a value unusable anywhere (short form, master) fails everywhere with the
// variable named, and an explicit flag keeps the env out entirely. The path
// argument is intentionally missing everywhere: with the flag override the
// first failure is the missing path, proving kube-version resolution passed.
func TestKubeVersionEnvFailsEveryCommand(t *testing.T) {
	missingPath := filepath.Join(t.TempDir(), "missing")
	cmds := map[string]func() *cobra.Command{
		"build": func() *cobra.Command {
			cmd := newBuildCmd()
			cmd.SetArgs([]string{"hr", "--path", missingPath})
			return cmd
		},
		"diff": func() *cobra.Command {
			cmd := newDiffCmd()
			cmd.SetArgs([]string{"hr", "--path", missingPath, "--branch-orig", "HEAD"})
			return cmd
		},
		"inventory": func() *cobra.Command {
			cmd := newInventoryCmd()
			cmd.SetArgs([]string{"--path", missingPath})
			return cmd
		},
		"validate": func() *cobra.Command {
			cmd := newValidateCmd()
			cmd.SetArgs([]string{"--path", missingPath})
			return cmd
		},
	}
	for _, env := range []string{"1.35", "master"} {
		for name, newCmd := range cmds {
			t.Run(name+" fails on env "+env, func(t *testing.T) {
				kubeVersionEnvMustFail(t, newCmd, env)
			})
		}
	}
	for name, newCmd := range cmds {
		t.Run(name+" skips the env with an explicit flag", func(t *testing.T) {
			kubeVersionEnvMustBeSkipped(t, newCmd, "1.35")
		})
	}
}

// kubeVersionEnvMustFail executes one command with an unusable env pin and
// requires the invalid-env error.
func kubeVersionEnvMustFail(t *testing.T, newCmd func() *cobra.Command, env string) {
	t.Helper()
	t.Setenv(kubeversion.EnvVar, env)
	cmd := newCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), `invalid FLUXVIEW_KUBE_VERSION "`+env+`"`) {
		t.Errorf("Execute() error = %v, want the invalid-env error", err)
	}
}

// kubeVersionEnvMustBeSkipped executes one command with an explicit
// --kubernetes-version: the unusable env pin must not be consulted — the
// missing-path failure proves kube-version resolution passed.
func kubeVersionEnvMustBeSkipped(t *testing.T, newCmd func() *cobra.Command, env string) {
	t.Helper()
	t.Setenv(kubeversion.EnvVar, env)
	cmd := newCmd()
	if err := cmd.Flags().Set("kubernetes-version", "1.36.1"); err != nil {
		t.Fatalf("set flag: %v", err)
	}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("missing path must fail the run")
	}
	if strings.Contains(err.Error(), "FLUXVIEW_KUBE_VERSION") {
		t.Errorf("the env must not be consulted when the flag is set, got: %v", err)
	}
}

// kubeVersionClusterFixture writes a minimal GitOps repository (a .git
// marker suffices for the repo-root lookup): a Flux Kustomization pointing
// at an apps dir whose HelmRelease renders a local kubeVersion-capped chart.
func kubeVersionClusterFixture(t *testing.T) string {
	t.Helper()
	repoRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), 0o755); err != nil {
		t.Fatalf("git marker: %v", err)
	}
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	cluster := filepath.Join(repoRoot, "clusters", "test")
	write(filepath.Join(cluster, "kustomization.yaml"), `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: kube-version
  namespace: flux-system
spec:
  interval: 5m
  path: ./apps
  sourceRef:
    kind: GitRepository
    name: flux-system
`)
	write(filepath.Join(repoRoot, "apps", "kustomization.yaml"), `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - helmrelease.yaml
`)
	write(filepath.Join(repoRoot, "apps", "helmrelease.yaml"), `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: capped
  namespace: kube-cap
spec:
  interval: 5m
  chart:
    spec:
      chart: ./apps/chart
      sourceRef:
        kind: GitRepository
        name: flux-system
`)
	write(filepath.Join(repoRoot, "apps", "chart", "Chart.yaml"), `apiVersion: v2
name: capped
version: 1.0.0
kubeVersion: < 1.37.0-0
`)
	write(filepath.Join(repoRoot, "apps", "chart", "values.yaml"), "")
	write(filepath.Join(repoRoot, "apps", "chart", "templates", "deployment.yaml"), `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
`)
	return cluster
}

// TestBuildFlagsKubeVersionMaster: --kubernetes-version master on a Helm
// command is rejected — charts render against concrete versions only; the
// loose flag grammar does not stretch to the schema-registry concept.
func TestBuildFlagsKubeVersionMaster(t *testing.T) {
	t.Setenv(kubeversion.EnvVar, "") // flag is explicit; the env stays out either way
	cmd := newBuildCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"hr", "--path", kubeVersionClusterFixture(t), "--kubernetes-version", "master"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), `invalid --kubernetes-version "master"`) {
		t.Errorf("Execute() error = %v, want the invalid flag error", err)
	}
}

// TestKubeVersionFlagRejectedWithoutHelmReleases: the flag is checked at
// the start of the command, not when the Helm renderer is created — build
// ks never creates one and would otherwise silently ignore the flag (and
// diff ks would only fail after the path checks).
func TestKubeVersionFlagRejectedWithoutHelmReleases(t *testing.T) {
	t.Setenv(kubeversion.EnvVar, "") // flag is explicit; the env stays out either way
	missingPath := filepath.Join(t.TempDir(), "missing")

	t.Run("build ks fails up front", func(t *testing.T) {
		cmd := newBuildCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs([]string{"ks", "--path", missingPath, "--kubernetes-version", "master"})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), `invalid --kubernetes-version "master"`) {
			t.Errorf("Execute() error = %v, want the invalid flag error before anything else runs", err)
		}
	})

	t.Run("diff ks fails up front", func(t *testing.T) {
		cmd := newDiffCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs([]string{"ks", "--path", missingPath, "--kubernetes-version", "master"})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), `invalid --kubernetes-version "master"`) {
			t.Errorf("Execute() error = %v, want the invalid flag error before anything else runs", err)
		}
	})

	t.Run("inventory crs fails up front", func(t *testing.T) {
		cmd := newInventoryCmd()
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs([]string{"crs", "--path", missingPath, "--kubernetes-version", "master"})
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), `invalid --kubernetes-version "master"`) {
			t.Errorf("Execute() error = %v, want the invalid flag error before anything else runs", err)
		}
	})
}

// TestKubeVersionWiring_InflationEndToEnd pins the issue #14 plumbing: the
// flag value carried in helmCacheOptions reaches the render capabilities —
// a chart capped below the assumed default fails, the same chart renders
// with an explicit --kubernetes-version override.
func TestKubeVersionWiring_InflationEndToEnd(t *testing.T) {
	repoRoot := t.TempDir()
	chartDir := filepath.Join(repoRoot, "charts", "capped")
	writeHelper(t, chartDir, "Chart.yaml", `apiVersion: v2
name: capped
version: 1.0.0
kubeVersion: < 1.29.0-0
`)
	writeHelper(t, chartDir, "values.yaml", "")
	writeHelper(t, filepath.Join(chartDir, "templates"), "dep.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: from-capped-chart
`)
	hr := []flux.HelmRelease{{
		Metadata: flux.ObjectMeta{Name: "app", Namespace: "apps"},
		Spec: flux.HelmReleaseSpec{
			Chart: flux.HelmReleaseChart{
				Spec: flux.HelmReleaseChartSpec{
					Chart: "./charts/capped",
					SourceRef: struct {
						Kind      string `yaml:"kind"`
						Name      string `yaml:"name"`
						Namespace string `yaml:"namespace,omitempty"`
					}{
						Kind: flux.KindGitRepository,
						Name: "flux-system",
					},
				},
			},
		},
	}}

	input := helmInflationInput{helmReleases: hr}
	// quiet=false so skip warnings (the signal under test) reach stderr.
	opts := inflateOptions{repoRoot: repoRoot}

	// Default (1.36.1): the cap at < 1.29.0-0 fails the render — a warning
	// and a skip in non-strict mode.
	inflater, err := helm.NewInflater(helmCacheOptions{dir: t.TempDir()}.inflaterOptions()...)
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}
	var outputs [][]byte
	stderr := captureStderr(func() {
		outputs, _ = inflateHelmReleasesShared(context.Background(), inflater, input, opts)
	})
	if len(outputs) != 0 {
		t.Fatalf("chart capped at < 1.29.0-0 must not render against the default:\n%s", outputs)
	}
	if !strings.Contains(stderr, "kubeVersion") {
		t.Errorf("expected a kubeVersion-cap warning, got:\n%s", stderr)
	}

	// Override: the same chart renders.
	overridden, err := helm.NewInflater(helmCacheOptions{dir: t.TempDir(), kubeVersion: "1.28.9"}.inflaterOptions()...)
	if err != nil {
		t.Fatalf("NewInflater with override: %v", err)
	}
	stderr = captureStderr(func() {
		outputs, _ = inflateHelmReleasesShared(context.Background(), overridden, input, opts)
	})
	if len(outputs) != 1 || !strings.Contains(string(outputs[0]), "from-capped-chart") {
		t.Errorf("expected the chart to render with --kubernetes-version 1.28.9 (stderr:\n%s)", stderr)
	}
}
