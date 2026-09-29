package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/banschikovde/fluxview/internal/inventory"
)

// TestInventoryCmd_Wiring runs the cobra command end to end (flag binding,
// subcommands, validation errors through the CLI surface).
func TestInventoryCmd_Wiring(t *testing.T) {
	f := newInventoryFixture(t)
	writeInventoryChart(t, filepath.Join(f.repoRoot, "charts", "podinfo"), "podinfo", "6.0.0", "6.0.0")
	f.addGitChartHR(t, "apps", "podinfo", "charts/podinfo", "6.0.0")
	f.addBaseKustomization(t, "podinfo.yaml")
	f.addKS(t, "../../apps/base")

	run := func(args ...string) (string, error) {
		t.Helper()
		root := &cobra.Command{Use: "fluxview"}
		root.AddCommand(newInventoryCmd())
		root.SetArgs(append([]string{"inventory"}, args...))
		var err error
		output := captureStdout(func() {
			err = root.Execute()
		})
		return output, err
	}

	output, err := run("--path", filepath.Join(f.repoRoot, f.clusterDir))
	if err != nil {
		t.Fatalf("inventory via cobra: %v", err)
	}
	if !strings.Contains(output, "podinfo") {
		t.Errorf("row missing via cobra:\n%s", output)
	}

	// Subcommand with its own flags.
	output, err = run("helm", "--path", filepath.Join(f.repoRoot, f.clusterDir))
	if err != nil {
		t.Fatalf("inventory helm via cobra: %v", err)
	}
	if !strings.Contains(output, "1 component (helm: 1)") {
		t.Errorf("helm subcommand output:\n%s", output)
	}

	// Unknown flag / bad value go through cobra + validation.
	if _, err = run("--path", filepath.Join(f.repoRoot, f.clusterDir), "--source", "bogus"); err == nil {
		t.Error("bad --source must fail")
	}
	if _, err = run("--path", filepath.Join(f.repoRoot, f.clusterDir), "--no-such-flag"); err == nil {
		t.Error("unknown flag must fail")
	}
}

// TestRunInventory_ChartRefOCINotFound covers the chartRef branch whose
// OCIRepository is not in the fleet: a warned row, no failure.
func TestRunInventory_ChartRefOCINotFound(t *testing.T) {
	f := newInventoryFixture(t)
	f.addManifest(t, "chartref.yaml", `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: oci-chart
  namespace: apps
spec:
  chartRef:
    kind: OCIRepository
    name: missing-repo
    namespace: flux-system
`)
	f.baseKustomization(t, "chartref.yaml")
	f.addKS(t, "../../apps/base")

	flags := f.flags()
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}
	if !strings.Contains(output, "oci-chart") {
		t.Errorf("chartRef row missing:\n%s", output)
	}
	if !strings.Contains(output, "1 warning") {
		t.Errorf("missing-repo warning expected:\n%s", output)
	}
}

// TestRunInventory_BucketSource checks that a Bucket-sourced HelmRelease
// keeps its spec chart version with a warning and appVersion "-".
func TestRunInventory_BucketSource(t *testing.T) {
	f := newInventoryFixture(t)
	f.addManifest(t, "bucket.yaml", `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: bucket-chart
  namespace: apps
spec:
  chart:
    spec:
      chart: internal-app
      version: "1.4.0"
      sourceRef:
        kind: Bucket
        name: charts-bucket
`)
	f.baseKustomization(t, "bucket.yaml")
	f.addKS(t, "../../apps/base")

	flags := f.flags()
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}
	if !strings.Contains(output, "bucket-chart") || !strings.Contains(output, "1.4.0") {
		t.Errorf("Bucket row must keep the spec version:\n%s", output)
	}
	if !strings.Contains(output, "1 warning") {
		t.Errorf("Bucket warning expected:\n%s", output)
	}
}

// TestRunInventory_MissingHelmRepository covers the HelmRepository-not-found
// branch: warned row, no failure.
func TestRunInventory_MissingHelmRepository(t *testing.T) {
	f := newInventoryFixture(t)
	f.addManifest(t, "hr.yaml", `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: orphan
  namespace: apps
spec:
  chart:
    spec:
      chart: some-chart
      version: "1.0.0"
      sourceRef:
        kind: HelmRepository
        name: no-such-repo
`)
	f.baseKustomization(t, "hr.yaml")
	f.addKS(t, "../../apps/base")

	flags := f.flags()
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}
	if !strings.Contains(output, "orphan") || !strings.Contains(output, "1 warning") {
		t.Errorf("missing HelmRepository row/warning:\n%s", output)
	}
}

// TestRunInventory_ImagesRenderFailure covers the --images degradation path:
// a chart whose metadata resolves but templates fail to render keeps its row
// with a "could not render images" warning.
func TestRunInventory_ImagesRenderFailure(t *testing.T) {
	f := newInventoryFixture(t)
	chartDir := filepath.Join(f.repoRoot, "charts", "broken")
	writeInventoryChart(t, chartDir, "broken", "1.0.0", "1.0.0")
	writeHelper(t, filepath.Join(chartDir, "templates"), "bad.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: bad
data:
  key: {{ .Values.nested.missing.deeply | functionThatDoesNotExist }}
`)
	f.addGitChartHR(t, "apps", "broken", "charts/broken", "1.0.0")
	f.addBaseKustomization(t, "broken.yaml")
	f.addKS(t, "../../apps/base")

	flags := f.flags()
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("render failure must degrade to a warning: %v", err)
	}
	if !strings.Contains(output, "broken") {
		t.Errorf("row missing after render failure:\n%s", output)
	}
	if !strings.Contains(output, "1 warning") {
		t.Errorf("render-failure warning expected:\n%s", output)
	}
	if !strings.Contains(output, "IMAGES") {
		t.Errorf("images column expected even on render failure:\n%s", output)
	}
}

// TestRunInventory_ChartlessHR covers the chartRef.kind=HelmChart / empty
// chart name branch.
func TestRunInventory_ChartlessHR(t *testing.T) {
	f := newInventoryFixture(t)
	f.addManifest(t, "chartless.yaml", `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: chartless
  namespace: apps
spec:
  chartRef:
    kind: HelmChart
    name: some-chart
`)
	f.baseKustomization(t, "chartless.yaml")
	f.addKS(t, "../../apps/base")

	flags := f.flags()
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}
	if !strings.Contains(output, "chartless") || !strings.Contains(output, "1 warning") {
		t.Errorf("chartRef HelmChart row/warning:\n%s", output)
	}
}

// TestRunInventory_ExplicitRulesFileMissing covers --rules pointing at a
// nonexistent file: exit 2.
func TestRunInventory_ExplicitRulesFileMissing(t *testing.T) {
	f := newInventoryFixture(t)
	writeInventoryChart(t, filepath.Join(f.repoRoot, "charts", "podinfo"), "podinfo", "6.0.0", "6.0.0")
	f.addGitChartHR(t, "apps", "podinfo", "charts/podinfo", "6.0.0")
	f.addBaseKustomization(t, "podinfo.yaml")
	f.addKS(t, "../../apps/base")

	flags := f.flags()
	flags.Rules = filepath.Join(f.repoRoot, "no-such-rules.yaml")
	err := runInventory(context.Background(), flags)
	if err == nil {
		t.Fatal("--rules with a missing file must fail")
	}
}

// TestRunInventory_SourceFilterEmptyResult covers --source selecting a source
// with no components.
func TestRunInventory_SourceFilterEmptyResult(t *testing.T) {
	f := newInventoryFixture(t)
	f.addManifest(t, "cr.yaml", `apiVersion: example.com/v1
kind: Widget
metadata:
  name: w1
  namespace: tools
`)
	f.baseKustomization(t, "cr.yaml")
	f.addKS(t, "../../apps/base")

	flags := f.flags()
	flags.Source = inventory.SourceHelm
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}
	if strings.Contains(output, "tools") {
		t.Errorf("helm-only filter must show no rows:\n%s", output)
	}
}
