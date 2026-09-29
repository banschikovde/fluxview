package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/banschikovde/fluxview/internal/inventory"
)

// writeInventoryChart creates a minimal local Helm chart at dir with the
// given name/version/appVersion.
func writeInventoryChart(t *testing.T, dir, name, version, appVersion string) {
	t.Helper()
	writeHelper(t, dir, "Chart.yaml", "apiVersion: v2\nname: "+name+"\nversion: "+version+"\nappVersion: "+appVersion+"\n")
	writeHelper(t, dir, "values.yaml", "")
}

// inventoryFixture builds a small GitOps repo: a Flux Kustomization in
// clusters/test pointing at apps/base which holds the given HR manifests,
// plus a local chart under charts/<name> referenced via GitRepository.
type inventoryFixture struct {
	repoRoot   string
	clusterDir string
}

func newInventoryFixture(t *testing.T) *inventoryFixture {
	t.Helper()
	f := &inventoryFixture{
		repoRoot:   t.TempDir(),
		clusterDir: filepath.Join("clusters", "test"),
	}
	// The repo root must be a git repository for FindRepoRoot.
	gitInit(t, f.repoRoot)
	return f
}

func (f *inventoryFixture) addKS(t *testing.T, path string) {
	t.Helper()
	writeHelper(t, filepath.Join(f.repoRoot, f.clusterDir), "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: apps
  namespace: flux-system
spec:
  path: ./`+path+`
  sourceRef:
    kind: GitRepository
    name: flux-system
`)
}

func (f *inventoryFixture) addGitChartHR(t *testing.T, ns, name, chartPath, version string) {
	t.Helper()
	writeHelper(t, filepath.Join(f.repoRoot, "apps", "base"), name+".yaml", `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: `+name+`
  namespace: `+ns+`
spec:
  chart:
    spec:
      chart: `+chartPath+`
      version: "`+version+`"
      sourceRef:
        kind: GitRepository
        name: flux-system
`)
}

// addBaseKustomization (re)writes the kustomization listing the HR files
// accumulated so far.
func (f *inventoryFixture) addBaseKustomization(t *testing.T, files ...string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n")
	for _, file := range files {
		b.WriteString("  - " + file + "\n")
	}
	writeHelper(t, filepath.Join(f.repoRoot, "apps", "base"), "kustomization.yaml", b.String())
}

func (f *inventoryFixture) flags() *InventoryFlags {
	return &InventoryFlags{
		Path:   filepath.Join(f.repoRoot, f.clusterDir),
		Output: "table",
		Sort:   inventory.DefaultSort,
	}
}

// TestRunInventory_HelmGitChart covers the MVP: one HelmRelease whose chart
// lives in the local checkout (GitRepository source) is rendered as a table
// row with chart name, resolved chart version and appVersion.
func TestRunInventory_HelmGitChart(t *testing.T) {
	f := newInventoryFixture(t)
	writeInventoryChart(t, filepath.Join(f.repoRoot, "charts", "podinfo"), "podinfo", "6.0.0", "6.0.0")
	f.addGitChartHR(t, "apps", "podinfo", "charts/podinfo", "6.0.0")
	f.addBaseKustomization(t, "podinfo.yaml")
	f.addKS(t, "../../apps/base")

	flags := f.flags()
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}

	if !strings.Contains(output, "helm") || !strings.Contains(output, "apps") || !strings.Contains(output, "podinfo") {
		t.Errorf("helm row missing in output:\n%s", output)
	}
	if !strings.Contains(output, "6.0.0") {
		t.Errorf("appVersion 6.0.0 missing:\n%s", output)
	}
	if !strings.Contains(output, "1 component (helm: 1)") {
		t.Errorf("summary line missing:\n%s", output)
	}
}

// TestRunInventory_HelmChartWarning checks that a HelmRelease whose chart
// cannot be resolved produces a warned row with the spec version, not a
// failed command.
func TestRunInventory_HelmChartWarning(t *testing.T) {
	f := newInventoryFixture(t)
	f.addGitChartHR(t, "apps", "ghost", "charts/no-such-chart", "1.0.0")
	f.addBaseKustomization(t, "ghost.yaml")
	f.addKS(t, "../../apps/base")

	flags := f.flags()
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory must not fail on unresolvable charts: %v", err)
	}

	if !strings.Contains(output, "ghost") {
		t.Errorf("row for unresolvable chart missing:\n%s", output)
	}
	if !strings.Contains(output, "1.0.0") {
		t.Errorf("spec chart version must still show:\n%s", output)
	}
	if !strings.Contains(output, "1 warning") {
		t.Errorf("warning count missing from summary:\n%s", output)
	}
}

// TestRunInventory_NamespaceFilter checks -n drops components outside the
// namespace.
func TestRunInventory_NamespaceFilter(t *testing.T) {
	f := newInventoryFixture(t)
	writeInventoryChart(t, filepath.Join(f.repoRoot, "charts", "a"), "a", "1.0.0", "1.0.0")
	writeInventoryChart(t, filepath.Join(f.repoRoot, "charts", "b"), "b", "2.0.0", "2.0.0")
	f.addGitChartHR(t, "team-a", "svc-a", "charts/a", "1.0.0")
	f.addGitChartHR(t, "team-b", "svc-b", "charts/b", "2.0.0")
	f.addBaseKustomization(t, "svc-a.yaml", "svc-b.yaml")
	f.addKS(t, "../../apps/base")

	flags := f.flags()
	flags.Namespace = "team-b"
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}

	if strings.Contains(output, "svc-a") {
		t.Errorf("team-a component leaked through namespace filter:\n%s", output)
	}
	if !strings.Contains(output, "svc-b") {
		t.Errorf("team-b component missing:\n%s", output)
	}
}

// TestRunInventory_NoKustomizations checks the error contract: a path
// without Flux Kustomizations fails with exit code 2.
func TestRunInventory_NoKustomizations(t *testing.T) {
	f := newInventoryFixture(t)

	flags := f.flags()
	err := runInventory(context.Background(), flags)
	if err == nil {
		t.Fatalf("runInventory must fail without Flux Kustomizations")
	}
	var exitErr *DiffExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode != ExitCodeError {
		t.Errorf("error must carry exit code %d, got %v", ExitCodeError, err)
	}
}

// TestRunInventory_InvalidFlags checks flag validation errors.
func TestRunInventory_InvalidFlags(t *testing.T) {
	f := newInventoryFixture(t)
	writeInventoryChart(t, filepath.Join(f.repoRoot, "charts", "podinfo"), "podinfo", "6.0.0", "6.0.0")
	f.addGitChartHR(t, "apps", "podinfo", "charts/podinfo", "6.0.0")
	f.addBaseKustomization(t, "podinfo.yaml")
	f.addKS(t, "../../apps/base")

	cases := []struct {
		name   string
		mutate func(*InventoryFlags)
	}{
		{"bad sort key", func(fl *InventoryFlags) { fl.Sort = "name,size" }},
		{"bad source", func(fl *InventoryFlags) { fl.Source = "helm,sidecar" }},
		{"bad output", func(fl *InventoryFlags) { fl.Output = "json" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags := f.flags()
			tc.mutate(flags)
			if err := runInventory(context.Background(), flags); err == nil {
				t.Fatalf("runInventory must reject %s", tc.name)
			}
		})
	}
}

// addManifest adds a plain manifest file to apps/base and registers it in
// the base kustomization.
func (f *inventoryFixture) addManifest(t *testing.T, file, content string) {
	t.Helper()
	writeHelper(t, filepath.Join(f.repoRoot, "apps", "base"), file, content)
}

func (f *inventoryFixture) baseKustomization(t *testing.T, files ...string) {
	f.addBaseKustomization(t, files...)
}

// writeInventoryRules writes a rules file into the fixture repo root and
// returns its path.
func (f *inventoryFixture) writeRules(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(f.repoRoot, "inventory-rules.yaml")
	writeHelper(t, f.repoRoot, "inventory-rules.yaml", content)
	return path
}

// TestRunInventory_CRs checks that CRs with matching rules become
// components with the rule's software name, extracted version and operator.
func TestRunInventory_CRs(t *testing.T) {
	f := newInventoryFixture(t)
	rulesFile := f.writeRules(t, `rules:
  - group: operator.victoriametrics.com
    kind: VMCluster
    software: VictoriaMetrics cluster
    operator: victoria-metrics-operator
    version:
      - jsonpath: .spec.clusterVersion
  - group: vault.banzaicloud.com
    kind: Vault
    software: Vault
    operator: vault-operator
    version:
      - jsonpath: .spec.image
        regex: ':(?P<version>[^@]+)'
  - group: kafka.strimzi.io
    kind: Kafka
    software: Kafka
    operator: strimzi
    version:
      - jsonpath: .spec.kafka.version
`)
	f.addManifest(t, "crs.yaml", `apiVersion: operator.victoriametrics.com/v1beta1
kind: VMCluster
metadata:
  name: vmcluster
  namespace: victoria-metrics
spec:
  clusterVersion: v1.146.0-cluster
---
apiVersion: vault.banzaicloud.com/v1alpha1
kind: Vault
metadata:
  name: vault
  namespace: vault
spec:
  image: hashicorp/vault:2.1.1
---
apiVersion: kafka.strimzi.io/v1beta2
kind: Kafka
metadata:
  name: strimzi
  namespace: kafka
spec:
  kafka:
    version: 3.8.0
`)
	f.baseKustomization(t, "crs.yaml")
	f.addKS(t, "../../apps/base")

	flags := f.flags()
	flags.Rules = rulesFile
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}

	if !strings.Contains(output, "vmcluster") || !strings.Contains(output, "victoria-metrics") {
		t.Errorf("VMCluster row missing:\n%s", output)
	}
	if !strings.Contains(output, "v1.146.0-cluster") {
		t.Errorf("VMCluster version missing:\n%s", output)
	}
	if !strings.Contains(output, "Vault") || !strings.Contains(output, "2.1.1") {
		t.Errorf("Vault row missing:\n%s", output)
	}
	if !strings.Contains(output, "3 components (cr: 3)") {
		t.Errorf("summary must count 3 cr components:\n%s", output)
	}
}

// TestRunInventory_CRDefaultVersion covers the fallback: a rule matched
// but no version field set → "default" with a warning.
func TestRunInventory_CRDefaultVersion(t *testing.T) {
	f := newInventoryFixture(t)
	rulesFile := f.writeRules(t, `rules:
  - group: operator.victoriametrics.com
    kind: VLAgent
    software: VictoriaLogs agent
    version:
      - jsonpath: .spec.image.tag
`)
	f.addManifest(t, "cr.yaml", `apiVersion: operator.victoriametrics.com/v1
kind: VLAgent
metadata:
  name: vlogs
  namespace: victoria-logs
spec: {}
`)
	f.baseKustomization(t, "cr.yaml")
	f.addKS(t, "../../apps/base")

	flags := f.flags()
	flags.Rules = rulesFile
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}
	if !strings.Contains(output, "default") {
		t.Errorf("default version marker missing:\n%s", output)
	}
	if !strings.Contains(output, "1 warning") {
		t.Errorf("warning for default version missing:\n%s", output)
	}
}

// TestRunInventory_AllCRs checks that rule-less CRs are hidden by default
// and shown with version unknown under --all-crs.
func TestRunInventory_AllCRs(t *testing.T) {
	f := newInventoryFixture(t)
	f.addManifest(t, "mixed.yaml", `apiVersion: example.com/v1
kind: Widget
metadata:
  name: mystery
  namespace: tools
spec:
  size: large
`)
	f.baseKustomization(t, "mixed.yaml")
	f.addKS(t, "../../apps/base")

	// Default: the Widget CR (no rule) is hidden.
	flags := f.flags()
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}
	if strings.Contains(output, "mystery") {
		t.Errorf("rule-less CR must be hidden by default:\n%s", output)
	}

	// --all-crs: the Widget shows with unknown.
	flags = f.flags()
	flags.AllCRs = true
	output = captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory --all-crs: %v", err)
	}
	if !strings.Contains(output, "mystery") || !strings.Contains(output, "unknown") {
		t.Errorf("rule-less CR must show as unknown with --all-crs:\n%s", output)
	}
}

// TestRunInventory_UserRules covers --rules: user-provided rules for
// arbitrary kinds drive the cr rows.
func TestRunInventory_UserRules(t *testing.T) {
	f := newInventoryFixture(t)
	f.addManifest(t, "cr.yaml", `apiVersion: example.com/v1
kind: Gadget
metadata:
  name: g1
  namespace: tools
spec:
  release: 2.1.0
---
apiVersion: operator.victoriametrics.com/v1beta1
kind: VMCluster
metadata:
  name: vmc
  namespace: monitoring
spec:
  retentionPeriod: 30d
`)
	f.baseKustomization(t, "cr.yaml")
	f.addKS(t, "../../apps/base")

	rulesFile := filepath.Join(f.repoRoot, "rules.yaml")
	writeHelper(t, filepath.Dir(rulesFile), "rules.yaml", `rules:
  - group: example.com
    kind: Gadget
    software: Gadget software
    version:
      - jsonpath: .spec.release
  - group: operator.victoriametrics.com
    kind: VMCluster
    software: VM cluster (custom rule)
    version:
      - jsonpath: .spec.retentionPeriod
`)

	flags := f.flags()
	flags.Rules = rulesFile
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}
	if !strings.Contains(output, "Gadget software") || !strings.Contains(output, "2.1.0") {
		t.Errorf("rule for one kind missing:\n%s", output)
	}
	if !strings.Contains(output, "30d") {
		t.Errorf("rule for the second kind missing:\n%s", output)
	}
}

// TestRunInventory_DefaultRulesPath covers the conventional
// .fluxview/inventory-rules.yaml picked up without --rules.
func TestRunInventory_DefaultRulesPath(t *testing.T) {
	f := newInventoryFixture(t)
	f.addManifest(t, "cr.yaml", `apiVersion: example.com/v1
kind: Widget
metadata:
  name: w1
  namespace: tools
spec:
  release: 9.9.9
`)
	f.baseKustomization(t, "cr.yaml")
	f.addKS(t, "../../apps/base")

	writeHelper(t, filepath.Join(f.repoRoot, ".fluxview"), "inventory-rules.yaml", `rules:
  - group: example.com
    kind: Widget
    software: Widget
    version:
      - jsonpath: .spec.release
`)

	flags := f.flags()
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}
	if !strings.Contains(output, "9.9.9") {
		t.Errorf("conventional rules file must apply:\n%s", output)
	}
}

// TestRunInventory_Manifests covers the manifest pipeline end to end:
// workloads from plain manifests become manifest components with the name
// label as the component name, image tag as the version, and the Flux
// Kustomization attribution.
func TestRunInventory_Manifests(t *testing.T) {
	f := newInventoryFixture(t)
	f.addManifest(t, "app.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: tools
  labels:
    app.kubernetes.io/name: echo-server
    app.kubernetes.io/version: 0.9.2
    kustomize.toolkit.fluxcd.io/name: apps
spec:
  template:
    spec:
      containers:
      - name: echo-server
        image: ghcr.io/echo/echo-server:0.9.2
`)
	f.baseKustomization(t, "app.yaml")
	f.addKS(t, "../../apps/base")

	flags := f.flags()
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}

	if !strings.Contains(output, "manifest") {
		t.Errorf("manifest source missing:\n%s", output)
	}
	if !strings.Contains(output, "echo-server") {
		t.Errorf("component named by label missing:\n%s", output)
	}
	if !strings.Contains(output, "0.9.2") {
		t.Errorf("image tag version missing:\n%s", output)
	}
	if !strings.Contains(output, "1 component (manifest: 1)") {
		t.Errorf("summary must count the manifest component:\n%s", output)
	}
}

// TestRunInventory_CRDGroups pins the lean crd behavior: CRD groups with a
// known version (label) become components; groups without one stay
// invisible; --source crd filters to them.
func TestRunInventory_CRDGroups(t *testing.T) {
	f := newInventoryFixture(t)
	f.addManifest(t, "crds.yaml", `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: vmclusters.operator.victoriametrics.com
  labels:
    app.kubernetes.io/version: v0.50.1
spec:
  group: operator.victoriametrics.com
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: vmsingles.operator.victoriametrics.com
  labels:
    app.kubernetes.io/version: v0.50.1
spec:
  group: operator.victoriametrics.com
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.com
spec:
  group: example.com
`)
	f.baseKustomization(t, "crds.yaml")
	f.addKS(t, "../../apps/base")

	flags := f.flags()
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}
	if !strings.Contains(output, "operator.victoriametrics.com (2 CRD)") || !strings.Contains(output, "v0.50.1") {
		t.Errorf("labeled CRD group missing:\n%s", output)
	}
	if !strings.Contains(output, "CRD groups (1)") {
		t.Errorf("crd section title missing:\n%s", output)
	}
	if strings.Contains(output, "example.com") {
		t.Errorf("CRD group without a version must stay invisible:\n%s", output)
	}
	if !strings.Contains(output, "1 component (crd: 1)") {
		t.Errorf("summary must count the crd component:\n%s", output)
	}

	// --source crd filters to the crd section only.
	flags = f.flags()
	flags.Source = "crd"
	output = captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory --source crd: %v", err)
	}
	if !strings.Contains(output, "operator.victoriametrics.com") || !strings.Contains(output, "1 component (crd: 1)") {
		t.Errorf("--source crd must keep the crd components:\n%s", output)
	}
}

// TestRunInventory_ImagesWithManifests covers --images for manifest
// components (images column from the workloads' own manifests) and for helm
// components (rendered chart).
func TestRunInventory_ImagesWithManifests(t *testing.T) {
	f := newInventoryFixture(t)
	writeInventoryChart(t, filepath.Join(f.repoRoot, "charts", "podinfo"), "podinfo", "6.0.0", "6.0.0")
	writeHelper(t, filepath.Join(f.repoRoot, "charts", "podinfo", "templates"), "dep.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}
spec:
  template:
    spec:
      containers:
      - name: app
        image: ghcr.io/stefanprodan/podinfo:6.0.0
`)
	f.addGitChartHR(t, "apps", "podinfo", "charts/podinfo", "6.0.0")
	f.addManifest(t, "app.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: tools
  labels:
    app.kubernetes.io/name: echo-server
spec:
  template:
    spec:
      containers:
      - name: echo-server
        image: ghcr.io/echo/echo-server:0.9.2
`)
	f.baseKustomization(t, "podinfo.yaml", "app.yaml")
	f.addKS(t, "../../apps/base")

	flags := f.flags()
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}

	if !strings.Contains(output, "IMAGES") {
		t.Errorf("images column missing:\n%s", output)
	}
	if !strings.Contains(output, "ghcr.io/echo/echo-server:0.9.2") {
		t.Errorf("manifest images missing:\n%s", output)
	}
	if !strings.Contains(output, "ghcr.io/stefanprodan/podinfo:6.0.0") {
		t.Errorf("rendered helm images missing:\n%s", output)
	}
	if !strings.Contains(output, "2 components (helm: 1, manifest: 1)") {
		t.Errorf("summary mismatch:\n%s", output)
	}
}

// TestRunInventory_CRsSubcommand covers `inventory crs`.
func TestRunInventory_CRsSubcommand(t *testing.T) {
	f := newInventoryFixture(t)
	writeInventoryChart(t, filepath.Join(f.repoRoot, "charts", "podinfo"), "podinfo", "6.0.0", "6.0.0")
	f.addGitChartHR(t, "apps", "podinfo", "charts/podinfo", "6.0.0")
	f.addManifest(t, "cr.yaml", `apiVersion: example.com/v1
kind: Widget
metadata:
  name: w1
  namespace: tools
spec:
  release: 9.9.9
`)
	f.baseKustomization(t, "podinfo.yaml", "cr.yaml")
	f.addKS(t, "../../apps/base")

	rulesFile := filepath.Join(f.repoRoot, "rules.yaml")
	writeHelper(t, filepath.Dir(rulesFile), "rules.yaml", `rules:
  - group: example.com
    kind: Widget
    software: Widget
    version:
      - jsonpath: .spec.release
`)

	flags := f.flags()
	flags.Source = inventory.SourceCR
	flags.Rules = rulesFile
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}
	if strings.Contains(output, "podinfo") {
		t.Errorf("helm components must be filtered out by --source cr:\n%s", output)
	}
	if !strings.Contains(output, "9.9.9") {
		t.Errorf("cr component missing:\n%s", output)
	}
	if !strings.Contains(output, "1 component (cr: 1)") {
		t.Errorf("summary must count only cr components:\n%s", output)
	}
}

// TestRunInventory_CRDVersionFromGitSource covers the git-tag path of crd
// versions: CRDs shipped by an external GitRepository with a pinned tag get
// that tag as their version (the CRD documents themselves carry no label).
func TestRunInventory_CRDVersionFromGitSource(t *testing.T) {
	// Upstream repository with CRDs under config/crds, tagged v1.0.0.
	upstreamDir := t.TempDir()
	gitInit(t, upstreamDir)
	if err := os.MkdirAll(filepath.Join(upstreamDir, "config", "crds"), 0755); err != nil {
		t.Fatal(err)
	}
	writeHelper(t, upstreamDir, "config/crds/kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - crd.yaml
`)
	writeHelper(t, upstreamDir, "config/crds/crd.yaml", `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: policies.kyverno.io
spec:
  group: kyverno.io
`)
	gitRun(t, upstreamDir, "add", "-A")
	gitRun(t, upstreamDir, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit", "-q", "-m", "crds")
	gitRun(t, upstreamDir, "tag", "v1.0.0")

	// Fleet repository: cluster path with the Flux KS + external GitRepository.
	f := newInventoryFixture(t)
	gitRun(t, f.repoRoot, "remote", "add", "origin", "https://example.com/org/fleet.git")
	writeHelper(t, filepath.Join(f.repoRoot, f.clusterDir), "gitrepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: kyverno-crds-src
  namespace: flux-system
spec:
  url: file://`+upstreamDir+`
  ref:
    tag: v1.0.0
`)
	writeHelper(t, filepath.Join(f.repoRoot, f.clusterDir), "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: kyverno-crds
  namespace: flux-system
spec:
  path: ./config/crds
  sourceRef:
    kind: GitRepository
    name: kyverno-crds-src
`)

	flags := f.flags()
	flags.KsCache.gitSourceDir = t.TempDir()
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}

	if !strings.Contains(output, "kyverno.io (1 CRD)") {
		t.Errorf("crd component missing:\n%s", output)
	}
	if !strings.Contains(output, "v1.0.0") {
		t.Errorf("version from the pinned git tag missing:\n%s", output)
	}
	if !strings.Contains(output, "1 component (crd: 1)") {
		t.Errorf("summary must count the crd component:\n%s", output)
	}
}

// TestRunInventory_CRDVersionFromFloatingRefInvisible pins the lean
// semantics on the negative path: a floating (branch) git ref contributes
// no version, so the CRD group stays invisible.
func TestRunInventory_CRDVersionFromFloatingRefInvisible(t *testing.T) {
	upstreamDir := t.TempDir()
	gitRun(t, upstreamDir, "init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(upstreamDir, "config", "crds"), 0755); err != nil {
		t.Fatal(err)
	}
	writeHelper(t, upstreamDir, "config/crds/kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - crd.yaml
`)
	writeHelper(t, upstreamDir, "config/crds/crd.yaml", `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: policies.kyverno.io
spec:
  group: kyverno.io
`)
	gitRun(t, upstreamDir, "add", "-A")
	gitRun(t, upstreamDir, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit", "-q", "-m", "crds")

	f := newInventoryFixture(t)
	gitRun(t, f.repoRoot, "remote", "add", "origin", "https://example.com/org/fleet.git")
	writeHelper(t, filepath.Join(f.repoRoot, f.clusterDir), "gitrepository.yaml", `apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: kyverno-crds-src
  namespace: flux-system
spec:
  url: file://`+upstreamDir+`
  ref:
    branch: main
`)
	writeHelper(t, filepath.Join(f.repoRoot, f.clusterDir), "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: kyverno-crds
  namespace: flux-system
spec:
  path: ./config/crds
  sourceRef:
    kind: GitRepository
    name: kyverno-crds-src
`)

	flags := f.flags()
	flags.KsCache.gitSourceDir = t.TempDir()
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}
	if strings.Contains(output, "kyverno.io") {
		t.Errorf("a floating ref contributes no version — the group must stay invisible:\n%s", output)
	}
	if !strings.Contains(output, "0 components") {
		t.Errorf("summary must be empty:\n%s", output)
	}
}

// TestRunInventory_BranchOrig covers --branch-orig: added, removed and
// updated components carry CHANGE markers; the exit code is 1 when versions
// changed.
func TestRunInventory_BranchOrig(t *testing.T) {
	f := newInventoryFixture(t)

	// Revision one: podinfo 6.0.0.
	writeInventoryChart(t, filepath.Join(f.repoRoot, "charts", "podinfo"), "podinfo", "6.0.0", "6.0.0")
	f.addGitChartHR(t, "apps", "podinfo", "charts/podinfo", "6.0.0")
	f.addBaseKustomization(t, "podinfo.yaml")
	f.addKS(t, "../../apps/base")
	gitRun(t, f.repoRoot, "add", "-A")
	gitRun(t, f.repoRoot, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit", "-q", "-m", "podinfo 6.0.0")

	// Working tree: podinfo bumped to 6.1.0, ghost component added.
	writeInventoryChart(t, filepath.Join(f.repoRoot, "charts", "podinfo"), "podinfo", "6.1.0", "6.1.0")
	f.addGitChartHR(t, "apps", "podinfo", "charts/podinfo", "6.1.0")
	f.addGitChartHR(t, "apps", "ghost", "charts/no-such-chart", "0.1.0")
	f.addBaseKustomization(t, "podinfo.yaml", "ghost.yaml")

	flags := f.flags()
	flags.BranchOrig = "HEAD"
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err == nil {
		t.Fatalf("--branch-orig with changes must exit non-zero")
	}
	var exitErr *DiffExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode != ExitDiffFound {
		t.Errorf("exit code must be %d (changes found), got %v", ExitDiffFound, err)
	}
	if !strings.Contains(output, "~ 6.0.0 → 6.1.0") {
		t.Errorf("updated marker missing:\n%s", output)
	}
	if !strings.Contains(output, "+") {
		t.Errorf("added marker missing:\n%s", output)
	}
	if !strings.Contains(output, "CHANGE") {
		t.Errorf("change column missing:\n%s", output)
	}
}

// TestRunInventory_BranchOrig_NoChanges covers the clean case: identical
// revisions exit 0.
func TestRunInventory_BranchOrig_NoChanges(t *testing.T) {
	f := newInventoryFixture(t)
	writeInventoryChart(t, filepath.Join(f.repoRoot, "charts", "podinfo"), "podinfo", "6.0.0", "6.0.0")
	f.addGitChartHR(t, "apps", "podinfo", "charts/podinfo", "6.0.0")
	f.addBaseKustomization(t, "podinfo.yaml")
	f.addKS(t, "../../apps/base")
	gitRun(t, f.repoRoot, "add", "-A")
	gitRun(t, f.repoRoot, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit", "-q", "-m", "podinfo 6.0.0")

	flags := f.flags()
	flags.BranchOrig = "HEAD"
	var err error
	_ = captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("identical revisions must exit 0, got %v", err)
	}
}

// TestRunInventory_MarkdownOutput covers -o markdown: GFM table on stdout.
func TestRunInventory_MarkdownOutput(t *testing.T) {
	f := newInventoryFixture(t)
	writeInventoryChart(t, filepath.Join(f.repoRoot, "charts", "podinfo"), "podinfo", "6.0.0", "6.0.0")
	f.addGitChartHR(t, "apps", "podinfo", "charts/podinfo", "6.0.0")
	f.addBaseKustomization(t, "podinfo.yaml")
	f.addKS(t, "../../apps/base")

	flags := f.flags()
	flags.Output = "markdown"
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}
	if !strings.HasPrefix(output, "## Helm releases (1)\n\n| Namespace |") {
		t.Errorf("markdown section header missing:\n%s", output)
	}
	if !strings.Contains(output, "| apps | podinfo | podinfo | 6.0.0 | 6.0.0 | none |") {
		t.Errorf("markdown row missing:\n%s", output)
	}
	if strings.Contains(output, "components (") {
		t.Errorf("markdown must not carry the summary line:\n%s", output)
	}
}

// TestRunInventory_Suspended checks suspended HelmReleases are skipped with
// a warning.
func TestRunInventory_Suspended(t *testing.T) {
	f := newInventoryFixture(t)
	writeInventoryChart(t, filepath.Join(f.repoRoot, "charts", "podinfo"), "podinfo", "6.0.0", "6.0.0")
	f.addGitChartHR(t, "apps", "podinfo", "charts/podinfo", "6.0.0")
	writeHelper(t, filepath.Join(f.repoRoot, "apps", "base"), "suspended.yaml", `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: paused
  namespace: apps
spec:
  suspend: true
  chart:
    spec:
      chart: charts/podinfo
      version: "6.0.0"
      sourceRef:
        kind: GitRepository
        name: flux-system
`)
	f.addBaseKustomization(t, "podinfo.yaml", "suspended.yaml")
	f.addKS(t, "../../apps/base")

	flags := f.flags()
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}
	if strings.Contains(output, "paused") {
		t.Errorf("suspended HelmRelease must not appear in inventory:\n%s", output)
	}
	if !strings.Contains(output, "podinfo") {
		t.Errorf("active HelmRelease missing:\n%s", output)
	}
}

// writeUmbrellaChart creates a local chart whose templates render two
// applications distinguished by the app.kubernetes.io/name label, plus a
// Vault CR (a CR created by a chart).
func writeUmbrellaChart(t *testing.T, dir string) {
	t.Helper()
	writeHelper(t, dir, "Chart.yaml", `apiVersion: v2
name: umbrella
version: 1.0.0
appVersion: 1.0.0
`)
	writeHelper(t, dir, "values.yaml", "")
	writeHelper(t, filepath.Join(dir, "templates"), "apps.yaml", `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  labels:
    app.kubernetes.io/name: frontend
    app.kubernetes.io/version: 2.1.0
spec:
  template:
    metadata:
      labels:
        app.kubernetes.io/name: frontend
    spec:
      containers:
      - name: frontend
        image: example.com/frontend:2.1.0
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: worker
  labels:
    app.kubernetes.io/name: backend
spec:
  template:
    metadata:
      labels:
        app.kubernetes.io/name: backend
    spec:
      containers:
      - name: backend
        image: example.com/backend:3.4.5
---
apiVersion: vault.banzaicloud.com/v1alpha1
kind: Vault
metadata:
  name: chart-vault
spec:
  image: hashicorp/vault:2.1.1
`)
}

// TestRunInventory_ChartCRComponent covers CRs rendered by a chart: they
// become cr rows with the PARENT column pointing at the HelmRelease, and
// dedup against fleet-collected CRs of the same identity.
func TestRunInventory_ChartCRComponent(t *testing.T) {
	f := newInventoryFixture(t)
	writeUmbrellaChart(t, filepath.Join(f.repoRoot, "charts", "umbrella"))
	f.addGitChartHR(t, "apps", "umbrella", "charts/umbrella", "1.0.0")
	f.addManifest(t, "fleet-vault.yaml", `apiVersion: vault.banzaicloud.com/v1alpha1
kind: Vault
metadata:
  name: fleet-vault
  namespace: vault
spec:
  image: hashicorp/vault:2.0.0
`)
	f.baseKustomization(t, "umbrella.yaml", "fleet-vault.yaml")
	f.addKS(t, "../../apps/base")
	rulesFile := f.writeRules(t, `rules:
  - group: vault.banzaicloud.com
    kind: Vault
    software: Vault
    operator: vault-operator
    version:
      - jsonpath: .spec.image
        regex: ':(?P<version>[^@]+)'
    image:
      - jsonpath: .spec.image
`)

	flags := f.flags()
	flags.Rules = rulesFile
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}

	if !strings.Contains(output, "chart-vault") {
		t.Errorf("chart-rendered CR row missing:\n%s", output)
	}
	if !strings.Contains(output, "umbrella") {
		t.Errorf("PARENT column must name the HelmRelease:\n%s", output)
	}
	if !strings.Contains(output, "fleet-vault") {
		t.Errorf("fleet CR row missing:\n%s", output)
	}
	if !strings.Contains(output, "3 components (helm: 1, cr: 2)") {
		t.Errorf("summary mismatch:\n%s", output)
	}
}

// TestRunInventory_SplitUmbrella covers --split-umbrella: an umbrella chart
// row splits into one row per application with the application's image tag
// as the version; without the flag the chart stays a single row.
func TestRunInventory_SplitUmbrella(t *testing.T) {
	f := newInventoryFixture(t)
	writeUmbrellaChart(t, filepath.Join(f.repoRoot, "charts", "umbrella"))
	f.addGitChartHR(t, "apps", "umbrella", "charts/umbrella", "1.0.0")
	f.baseKustomization(t, "umbrella.yaml")
	f.addKS(t, "../../apps/base")

	// Default: one compact row for the whole chart.
	flags := f.flags()
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory: %v", err)
	}
	if !strings.Contains(output, "umbrella  umbrella  1.0.0") && !strings.Contains(output, "umbrella         umbrella") {
		t.Errorf("default must keep a single chart row:\n%s", output)
	}
	if strings.Contains(output, "umbrella/frontend") {
		t.Errorf("no split without the flag:\n%s", output)
	}

	// --split-umbrella: one row per application, version = image tag.
	flags = f.flags()
	flags.SplitUmbrella = true
	output = captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("runInventory --split-umbrella: %v", err)
	}
	if !strings.Contains(output, "umbrella/frontend") || !strings.Contains(output, "2.1.0") {
		t.Errorf("frontend child row missing:\n%s", output)
	}
	if !strings.Contains(output, "umbrella/backend") || !strings.Contains(output, "3.4.5") {
		t.Errorf("backend child row missing:\n%s", output)
	}
	if strings.Contains(output, "1 component (helm: 1)") {
		t.Errorf("split must count the children:\n%s", output)
	}
}

// TestRunInventory_BranchOrigRenderFailureSymmetric pins the P2 fix: a
// chart that renders on the current side but fails on the comparison side
// must not fabricate chart-derived rows (+ on CRs / umbrella children) in
// the diff — its derived rows drop from both sides, exit stays 0.
func TestRunInventory_BranchOrigRenderFailureSymmetric(t *testing.T) {
	f := newInventoryFixture(t)
	// Revision one: the chart (version 1.0.0) renders two apps and a
	// Vault CR.
	writeUmbrellaChart(t, filepath.Join(f.repoRoot, "charts", "umbrella"))
	f.addGitChartHR(t, "apps", "umbrella", "charts/umbrella", "1.0.0")
	f.baseKustomization(t, "umbrella.yaml")
	f.addKS(t, "../../apps/base")
	rulesFile := f.writeRules(t, `rules:
  - group: vault.banzaicloud.com
    kind: Vault
    software: Vault
    version:
      - jsonpath: .spec.image
        regex: ':(?P<version>[^@]+)'
`)
	gitRun(t, f.repoRoot, "add", "-A")
	gitRun(t, f.repoRoot, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit", "-q", "-m", "umbrella 1.0.0")

	// Working tree: same chart version and Chart.yaml (metadata — and thus
	// the helm row — is identical on both sides), but a template that no
	// longer renders. Chart-derived rows exist only on the comparison
	// side; without the symmetric drop they would surface as "+" and flip
	// the exit code.
	writeHelper(t, filepath.Join(f.repoRoot, "charts", "umbrella", "templates"), "apps.yaml", `apiVersion: v1
kind: ConfigMap
metadata:
  name: broken
data:
  key: {{ noSuchTemplateFunction .Values }}
`)

	// SplitUmbrella ON: the release's umbrella children are chart-derived
	// rows too and must drop symmetrically, exactly like the chart CR.
	flags := f.flags()
	flags.Rules = rulesFile
	flags.SplitUmbrella = true
	flags.BranchOrig = "HEAD"
	var err error
	output := captureStdout(func() {
		err = runInventory(context.Background(), flags)
	})
	if err != nil {
		t.Fatalf("--branch-orig with a one-sided render failure must not report changes: %v", err)
	}
	if strings.Contains(output, "+") || strings.Contains(output, "~") || strings.Contains(output, "-") {
		t.Errorf("one-sided render failure must not fabricate changes:\n%s", output)
	}
	if strings.Contains(output, "chart-vault") {
		t.Errorf("chart-derived CR row must drop from both sides:\n%s", output)
	}
	if strings.Contains(output, "umbrella/frontend") || strings.Contains(output, "umbrella/backend") {
		t.Errorf("umbrella children of the failed release must drop from both sides:\n%s", output)
	}
}

// TestUmbrellaChildrenWarningsCopy pins the slice-copy in umbrellaChildren:
// appending to one child's warnings must never write through a shared
// backing array into a sibling.
func TestUmbrellaChildrenWarningsCopy(t *testing.T) {
	parent := inventory.Component{
		Source: inventory.SourceHelm, Namespace: "apps", Name: "umbrella",
		// Preallocated capacity: without the copy, cap > len would let a
		// sibling's append land in the same backing array.
		Warnings: append(make([]string, 0, 8), "chart note"),
	}
	docs := []renderedDoc{
		{apiVersion: "apps/v1", kind: "Deployment", raw: map[string]interface{}{
			"kind": "Deployment",
			"metadata": map[string]interface{}{
				"name": "web", "namespace": "apps",
				"labels": map[string]interface{}{"app.kubernetes.io/name": "frontend"},
			},
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"containers": []interface{}{map[string]interface{}{
							"name": "frontend", "image": "example.com/frontend:2.1.0",
						}},
					},
				},
			},
		}},
		{apiVersion: "apps/v1", kind: "Deployment", raw: map[string]interface{}{
			"kind": "Deployment",
			"metadata": map[string]interface{}{
				"name": "worker", "namespace": "apps",
				"labels": map[string]interface{}{"app.kubernetes.io/name": "backend"},
			},
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"containers": []interface{}{map[string]interface{}{
							"name": "backend", "image": "example.com/backend:3.4.5",
						}},
					},
				},
			},
		}},
	}
	children, ok := umbrellaChildren(parent, docs)
	if !ok || len(children) != 2 {
		t.Fatalf("expected 2 children, got %d (ok=%v)", len(children), ok)
	}
	for _, c := range children {
		if len(c.Warnings) != 1 || c.Warnings[0] != "chart note" {
			t.Fatalf("child must inherit the parent warning by copy: %v", c.Warnings)
		}
	}
	children[0].Warnings = append(children[0].Warnings, "extra")
	if len(children[1].Warnings) != 1 {
		t.Errorf("append on one child leaked into the sibling: %v", children[1].Warnings)
	}
}
