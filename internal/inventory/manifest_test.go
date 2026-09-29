package inventory

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func parseDocs(t *testing.T, docs string) []map[string]interface{} {
	t.Helper()
	var out []map[string]interface{}
	for _, doc := range strings.Split(docs, "\n---\n") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var raw map[string]interface{}
		if err := yaml.Unmarshal([]byte(doc), &raw); err != nil {
			t.Fatalf("parsing doc: %v", err)
		}
		out = append(out, raw)
	}
	return out
}

func TestCollectManifestComponents_NamePriority(t *testing.T) {
	// Component name priority: app.kubernetes.io/name > part-of > workload name.
	inputs := []ManifestInput{
		{Raw: parseDocs(t, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web-5x8k
  namespace: tools
  labels:
    app.kubernetes.io/name: echo-server
    app.kubernetes.io/version: 0.9.2
spec:
  template:
    spec:
      containers:
      - name: echo-server
        image: ghcr.io/echo/echo-server:0.9.2
`)[0]},
		{Raw: parseDocs(t, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: worker-dep
  namespace: jobs
  labels:
    app.kubernetes.io/part-of: pipeline
spec:
  template:
    spec:
      containers:
      - name: runner
        image: runner:2.0
`)[0]},
		{Raw: parseDocs(t, `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: node-exporter
  namespace: monitoring
spec:
  template:
    spec:
      containers:
      - name: node-exporter
        image: prometheus/node-exporter:v1.8.2
`)[0]},
	}

	components := CollectManifestComponents(inputs)
	if len(components) != 3 {
		t.Fatalf("got %d components, want 3: %+v", len(components), components)
	}
	byName := map[string]Component{}
	for _, c := range components {
		byName[c.Name] = c
	}
	if _, ok := byName["echo-server"]; !ok {
		t.Errorf("name label must win: %+v", byName)
	}
	if _, ok := byName["pipeline"]; !ok {
		t.Errorf("part-of label must win: %+v", byName)
	}
	if _, ok := byName["node-exporter"]; !ok {
		t.Errorf("workload name fallback missing: %+v", byName)
	}
	if byName["echo-server"].Version != "0.9.2" {
		t.Errorf("version = %q, want 0.9.2", byName["echo-server"].Version)
	}
	if byName["pipeline"].Version != "2.0" {
		t.Errorf("version = %q, want 2.0", byName["pipeline"].Version)
	}
}

func TestCollectManifestComponents_GroupingAndVersion(t *testing.T) {
	// A Deployment and a CronJob sharing the name label merge into one
	// component; the Deployment (higher workload priority) supplies the
	// version; images aggregate.
	inputs := []ManifestInput{
		{Raw: parseDocs(t, `apiVersion: batch/v1
kind: CronJob
metadata:
  name: cleanup
  namespace: tools
  labels:
    app.kubernetes.io/name: echo-server
spec:
  schedule: "* * * * *"
  jobTemplate:
    spec:
      template:
        spec:
          containers:
          - name: echo-server
            image: ghcr.io/echo/echo-server:0.9.0
`)[0]},
		{Raw: parseDocs(t, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: tools
  labels:
    app.kubernetes.io/name: echo-server
    app.kubernetes.io/part-of: web-stack
    kustomize.toolkit.fluxcd.io/name: apps
spec:
  template:
    spec:
      containers:
      - name: istio-proxy
        image: istio/proxyv:1.22.0
      - name: echo-server
        image: ghcr.io/echo/echo-server:0.9.2
      - name: sidecar-tool
        image: helper:1.1
      initContainers:
      - name: init-thing
        image: initimg:2.2
`)[0], FluxKs: "apps"},
	}

	components := CollectManifestComponents(inputs)
	if len(components) != 1 {
		t.Fatalf("got %d components, want 1: %+v", len(components), components)
	}
	c := components[0]
	if c.Version != "0.9.2" {
		t.Errorf("version = %q, want 0.9.2 (Deployment wins over CronJob)", c.Version)
	}
	if c.VersionSource != VersionImage {
		t.Errorf("versionSource = %q, want image", c.VersionSource)
	}
	if c.FluxKs != "apps" {
		t.Errorf("FluxKs = %q, want apps (attribution label)", c.FluxKs)
	}
	// Sidecars excluded, init containers included.
	joined := strings.Join(c.Images, ",")
	if !strings.Contains(joined, "ghcr.io/echo/echo-server:0.9.2") {
		t.Errorf("main image missing from images: %v", c.Images)
	}
	if !strings.Contains(joined, "helper:1.1") || !strings.Contains(joined, "initimg:2.2") {
		t.Errorf("regular/init images missing: %v", c.Images)
	}
	if strings.Contains(joined, "istio/proxyv") {
		t.Errorf("sidecar image must be excluded: %v", c.Images)
	}
	// Diverging versions across the group warn.
	found := false
	for _, w := range c.Warnings {
		if strings.Contains(w, "different versions") {
			found = true
		}
	}
	if !found {
		t.Errorf("diverging group versions must warn: %v", c.Warnings)
	}
}

func TestCollectManifestComponents_VersionFallbacks(t *testing.T) {
	// latest tag → version label; no tag → digest prefix.
	inputs := []ManifestInput{
		{Raw: parseDocs(t, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: latest-app
  namespace: a
  labels:
    app.kubernetes.io/version: 1.2.3
spec:
  template:
    spec:
      containers:
      - name: latest-app
        image: repo/app:latest
`)[0]},
		{Raw: parseDocs(t, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: digest-app
  namespace: b
spec:
  template:
    spec:
      containers:
      - name: digest-app
        image: repo/app@sha256:0123456789abcdef0123456789abcdef
`)[0]},
		{Raw: parseDocs(t, `apiVersion: apps/v1
kind: Deployment
metadata:
  name: noversion-app
  namespace: c
spec:
  template:
    spec:
      containers:
      - name: noversion-app
        image: repo/app
`)[0]},
	}

	components := CollectManifestComponents(inputs)
	if len(components) != 3 {
		t.Fatalf("got %d components, want 3", len(components))
	}
	byNS := map[string]Component{}
	for _, c := range components {
		byNS[c.Namespace] = c
	}
	if byNS["a"].Version != "1.2.3" || byNS["a"].VersionSource != VersionLabel {
		t.Errorf("latest → label fallback broken: %+v", byNS["a"])
	}
	if byNS["b"].Version != "0123456789ab" {
		t.Errorf("digest prefix = %q, want 0123456789ab", byNS["b"].Version)
	}
	if len(byNS["b"].Warnings) == 0 {
		t.Errorf("digest fallback must warn")
	}
	if byNS["c"].Version != "" && !strings.Contains(strings.Join(byNS["c"].Warnings, ";"), "no version") {
		t.Errorf("no-version workload must warn: %+v", byNS["c"])
	}
}

func TestCollectManifestComponents_NonWorkloadsSkipped(t *testing.T) {
	inputs := []ManifestInput{
		{Raw: parseDocs(t, `apiVersion: v1
kind: Service
metadata: {name: svc, namespace: tools}
`)[0]},
		{Raw: parseDocs(t, `apiVersion: v1
kind: ConfigMap
metadata: {name: cm, namespace: tools}
`)[0]},
		{Raw: map[string]interface{}{"kind": "Deployment", "metadata": map[string]interface{}{}}},
	}
	components := CollectManifestComponents(inputs)
	if len(components) != 0 {
		t.Errorf("non-workload documents must be skipped, got %+v", components)
	}
}

func TestSplitImage(t *testing.T) {
	cases := []struct {
		image             string
		repo, tag, digest string
	}{
		{"repo/app:1.0", "repo/app", "1.0", ""},
		{"registry:5000/repo/app:2.0", "registry:5000/repo/app", "2.0", ""},
		{"registry:5000/repo/app", "registry:5000/repo/app", "", ""},
		{"repo/app@sha256:abc", "repo/app", "", "sha256:abc"},
		{"repo/app:1.2@sha256:abc", "repo/app", "1.2", "sha256:abc"},
		{"app", "app", "", ""},
	}
	for _, tc := range cases {
		repo, tag, digest := SplitImage(tc.image)
		if repo != tc.repo || tag != tc.tag || digest != tc.digest {
			t.Errorf("SplitImage(%q) = %q, %q, %q; want %q, %q, %q",
				tc.image, repo, tag, digest, tc.repo, tc.tag, tc.digest)
		}
	}
}

func TestCRDComponents_Lean(t *testing.T) {
	raws := parseDocs(t, `apiVersion: apiextensions.k8s.io/v1
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
  name: gadgets.example.com
spec:
  group: example.com
`)

	components := CRDComponents(raws, map[string]string{
		"example.com": "v2.0.0", // git tag fallback for the label-less group
	})
	if len(components) != 2 {
		t.Fatalf("got %d crd components, want 2: %+v", len(components), components)
	}

	byGroup := map[string]Component{}
	for _, c := range components {
		group := c.Name
		if i := strings.Index(c.Name, " ("); i >= 0 {
			group = c.Name[:i]
		}
		byGroup[group] = c
	}

	vm := byGroup["operator.victoriametrics.com"]
	if vm.Version != "v0.50.1" || vm.VersionSource != VersionLabel {
		t.Errorf("label version = %+v", vm)
	}
	if !strings.Contains(vm.Name, "(2 CRD)") {
		t.Errorf("grouped name = %q, want count suffix", vm.Name)
	}

	ex := byGroup["example.com"]
	if ex.Version != "v2.0.0" || ex.VersionSource != VersionGitRef {
		t.Errorf("git-ref version = %+v", ex)
	}
}

func TestCRDComponents_UnknownGroupsInvisible(t *testing.T) {
	raws := parseDocs(t, `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.com
spec:
  group: example.com
`)
	if components := CRDComponents(raws, nil); len(components) != 0 {
		t.Errorf("groups without a version must stay invisible, got %+v", components)
	}
}

func TestCRDComponents_DivergingVersions(t *testing.T) {
	raws := parseDocs(t, `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: a.example.com
  labels:
    app.kubernetes.io/version: v1.2.0
spec:
  group: example.com
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: b.example.com
  labels:
    app.kubernetes.io/version: v1.10.0
spec:
  group: example.com
`)
	components := CRDComponents(raws, nil)
	if len(components) != 1 {
		t.Fatalf("got %d components, want 1", len(components))
	}
	// v1.10.0 > v1.2.0 semver-wise (string compare would pick v1.2.0).
	if components[0].Version != "v1.10.0" {
		t.Errorf("highest version = %q, want v1.10.0", components[0].Version)
	}
	if len(components[0].Warnings) == 0 {
		t.Errorf("diverging CRD versions must warn")
	}
}

func TestTable_CRDSection(t *testing.T) {
	components := []Component{
		{Source: SourceCRD, Kind: "CustomResourceDefinition", Name: "kyverno.io (7 CRD)", Version: "v1.19.1", VersionSource: VersionGitRef},
	}
	out := Table(components, RenderOptions{Headers: true})
	if !strings.Contains(out, "CRD groups (1)") {
		t.Errorf("crd section title missing:\n%s", out)
	}
	if !strings.Contains(out, "NAME") || !strings.Contains(out, "VERSION") {
		t.Errorf("crd columns missing:\n%s", out)
	}
	if strings.Contains(out, "NAMESPACE") || strings.Contains(out, "CHART") {
		t.Errorf("crd section must not carry namespace/chart columns:\n%s", out)
	}
	if !strings.Contains(out, "kyverno.io (7 CRD)  v1.19.1") {
		t.Errorf("crd row missing:\n%s", out)
	}

	// Markdown mirrors the section.
	md := Markdown(components, RenderOptions{Headers: true})
	if !strings.Contains(md, "## CRD groups (1)") || !strings.Contains(md, "| kyverno.io (7 CRD) | v1.19.1 |") {
		t.Errorf("markdown crd section missing:\n%s", md)
	}
}

func TestHighestVersion(t *testing.T) {
	cases := []struct {
		name     string
		versions []string
		want     string
	}{
		{"parsable outranks unparsable", []string{"zzz", "1.0.0"}, "1.0.0"},
		{"parsable leader keeps rank", []string{"1.0.0", "zzz"}, "1.0.0"},
		{"non-semver does not beat semver", []string{"2.0.0.1", "2.0.0"}, "2.0.0"},
		{"semver over lexicographic", []string{"v1.2.0", "v1.10.0"}, "v1.10.0"},
		{"semver-equal deterministic tie-break", []string{"1.0", "1.0.0"}, "1.0.0"},
		{"semver-equal tie-break both orders", []string{"1.0.0", "1.0"}, "1.0.0"},
		{"unparsable fallback to string", []string{"alpha", "zzz"}, "zzz"},
		{"single", []string{"only"}, "only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := map[string]bool{}
			for _, v := range tc.versions {
				set[v] = true
			}
			if got := highestVersion(set); got != tc.want {
				t.Errorf("highestVersion(%v) = %q, want %q", tc.versions, got, tc.want)
			}
		})
	}
}
