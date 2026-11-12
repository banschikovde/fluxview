package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/helm"
	"github.com/banschikovde/fluxview/internal/validate"
)

// TestKubeVersionFlagWiring verifies the --kubernetes-version flag plumbing
// of the Helm-inflating commands (build, diff, inventory) and that validate
// reads the same FLUXVIEW_KUBE_VERSION env (strictly parsed): registration,
// an env-driven default and the fallback for an unusable env value. The
// flag value's effect on rendering is covered end to end by
// TestKubeVersionWiring_InflationEndToEnd below.
func TestKubeVersionFlagWiring(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"registered on build, diff and inventory", kubeVersionWiringRegistered},
		{"default is validate's version and honors the env", kubeVersionWiringDefaultFromEnv},
		{"invalid env falls back to the default", kubeVersionWiringInvalidEnvFallback},
		{"validate shares the same env knob", kubeVersionWiringValidateEnv},
	} {
		t.Run(tc.name, tc.run)
	}
}

// kubeVersionWiringRegistered: every Helm-inflating command must register
// the flag.
func kubeVersionWiringRegistered(t *testing.T) {
	for name, cmd := range map[string]*cobra.Command{
		"build": newBuildCmd(), "diff": newDiffCmd(), "inventory": newInventoryCmd(),
	} {
		if cmd.Flags().Lookup("kubernetes-version") == nil {
			t.Errorf("%s must register --kubernetes-version", name)
		}
	}
}

// kubeVersionWiringDefaultFromEnv: unset env → the validate-synced default;
// a set env drives the flag default of all three commands.
func kubeVersionWiringDefaultFromEnv(t *testing.T) {
	t.Setenv("FLUXVIEW_KUBE_VERSION", "")
	def := newBuildCmd().Flags().Lookup("kubernetes-version").DefValue
	if def != validate.DefaultKubernetesVersion {
		t.Errorf("build flag default = %s, want validate.DefaultKubernetesVersion (%s) — the two must stay in sync", def, validate.DefaultKubernetesVersion)
	}

	t.Setenv("FLUXVIEW_KUBE_VERSION", "1.30.2")
	for name, cmd := range map[string]*cobra.Command{
		"build": newBuildCmd(), "diff": newDiffCmd(), "inventory": newInventoryCmd(),
	} {
		if got := cmd.Flags().Lookup("kubernetes-version").DefValue; got != "1.30.2" {
			t.Errorf("%s flag default = %s, want the FLUXVIEW_KUBE_VERSION value 1.30.2", name, got)
		}
	}
}

// kubeVersionWiringInvalidEnvFallback: an unusable env value falls back to
// the default at flag registration.
func kubeVersionWiringInvalidEnvFallback(t *testing.T) {
	t.Setenv("FLUXVIEW_KUBE_VERSION", "bogus")
	if got := newDiffCmd().Flags().Lookup("kubernetes-version").DefValue; got != validate.DefaultKubernetesVersion {
		t.Errorf("diff flag default = %s, want the default %s for an invalid env value", got, validate.DefaultKubernetesVersion)
	}
}

// kubeVersionWiringValidateEnv: validate reads the same env with its own
// strict parsing — a short form ("1.35") is unusable for schema URLs and
// falls back to the default instead.
func kubeVersionWiringValidateEnv(t *testing.T) {
	def := func() string {
		return newValidateCmd().Flags().Lookup("kubernetes-version").DefValue
	}

	t.Setenv("FLUXVIEW_KUBE_VERSION", "")
	if got := def(); got != validate.DefaultKubernetesVersion {
		t.Errorf("validate flag default = %s, want the default %s when env unset", got, validate.DefaultKubernetesVersion)
	}

	t.Setenv("FLUXVIEW_KUBE_VERSION", "1.35.7")
	if got := def(); got != "1.35.7" {
		t.Errorf("validate flag default = %s, want the FLUXVIEW_KUBE_VERSION value 1.35.7", got)
	}

	t.Setenv("FLUXVIEW_KUBE_VERSION", "1.35")
	if got := def(); got != validate.DefaultKubernetesVersion {
		t.Errorf("validate flag default = %s, want the default %s for a short-form env value", got, validate.DefaultKubernetesVersion)
	}
}

// TestKubeVersionWiring_InflationEndToEnd pins the issue #14 plumbing: the
// flag value carried in helmCacheOptions reaches the render capabilities —
// a chart capped below the assumed default fails, the same chart renders
// with an explicit --kubernetes-version override.
func TestKubeVersionWiring_InflationEndToEnd(t *testing.T) {
	t.Setenv("FLUXVIEW_KUBE_VERSION", "") // pin the default regardless of the outer environment

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
