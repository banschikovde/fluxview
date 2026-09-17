package validate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// localLocation builds a kubeconform location template for a local directory
// with schemas at its root (<kind>-<group>-<version>.json layout).
func localLocation(dir string) []string {
	return []string{filepath.Join(dir, "{{ .ResourceKind }}{{ .KindSuffix }}.json")}
}

func TestValidate_JSONSchemaDir_Valid(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "widget-test-v1.json", testWidgetSchema)

	v, err := New(Options{SchemaLocations: localLocation(dir)})
	if err != nil {
		t.Fatal(err)
	}
	results := v.Validate([]byte(`
apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: my-widget
  namespace: default
spec:
  size: large
  color: red
`))

	if len(results) != 0 {
		t.Errorf("expected 0 validation errors, got %d: %+v", len(results), results)
	}
}

func TestValidate_JSONSchemaDir_MissingRequired(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "widget-test-v1.json", testWidgetSchema)

	v, err := New(Options{SchemaLocations: localLocation(dir)})
	if err != nil {
		t.Fatal(err)
	}
	results := v.Validate([]byte(`
apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: my-widget
spec:
  size: large
`))

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d: %+v", len(results), results)
	}
	if !strings.Contains(strings.Join(results[0].Errors, " "), "color") {
		t.Errorf("expected a missing 'color' error, got: %v", results[0].Errors)
	}
	if results[0].Label() != "Widget my-widget" {
		t.Errorf("resource label = %q, want Widget my-widget", results[0].Label())
	}
}

func TestValidate_JSONSchemaDir_EnumViolation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "widget-test-v1.json", testWidgetSchema)

	v, err := New(Options{SchemaLocations: localLocation(dir)})
	if err != nil {
		t.Fatal(err)
	}
	results := v.Validate([]byte(`
apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: my-widget
spec:
  size: enormous
  color: red
`))

	if len(results) == 0 {
		t.Fatal("expected validation error for enum violation")
	}
}

func TestValidate_FluxSchemaNaming(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "helmrelease-helm-v2.json", `{
	"type": "object",
	"properties": {
		"spec": {"type": "object", "required": ["interval"], "properties": {
			"interval": {"type": "string"}
		}}
	}
}`)

	v, err := New(Options{SchemaLocations: localLocation(dir)})
	if err != nil {
		t.Fatal(err)
	}

	// The lowercase filename kind matches the CamelCase resource kind.
	results := v.Validate([]byte(`
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: my-release
  namespace: flux-system
spec:
  interval: 5m
`))
	if len(results) != 0 {
		t.Errorf("expected 0 validation errors, got %d: %+v", len(results), results)
	}

	results = v.Validate([]byte(`
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: my-release
spec: {}
`))
	if len(results) == 0 {
		t.Fatal("expected validation error for missing spec.interval")
	}
}

func TestValidate_CoreGroupNaming(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "configmap-v1.json", `{
	"type": "object",
	"properties": {
		"data": {"type": "object", "additionalProperties": {"type": "string"}}
	}
}`)

	v, err := New(Options{SchemaLocations: localLocation(dir)})
	if err != nil {
		t.Fatal(err)
	}

	results := v.Validate([]byte(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  key: value
`))
	if len(results) != 0 {
		t.Errorf("expected 0 validation errors, got %d: %+v", len(results), results)
	}

	results = v.Validate([]byte(`
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
data:
  key: 42
`))
	if len(results) == 0 {
		t.Fatal("expected validation error for an integer ConfigMap data value")
	}
}

func TestValidate_MultipleLocations_FirstWins(t *testing.T) {
	strict := t.TempDir()
	writeFile(t, strict, "widget-test-v1.json", testWidgetSchema)
	lenient := t.TempDir()
	writeFile(t, lenient, "widget-test-v1.json", `{"type": "object"}`)

	v, err := New(Options{SchemaLocations: append(localLocation(strict), localLocation(lenient)...)})
	if err != nil {
		t.Fatal(err)
	}

	// Missing spec.color must fail against the strict schema of the first
	// location, not pass against the lenient second one.
	results := v.Validate([]byte(`
apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: my-widget
spec:
  size: large
`))
	if len(results) == 0 {
		t.Fatal("expected the first location's schema to win")
	}
}

func TestValidate_NoSchema_SkipsSilently(t *testing.T) {
	dir := t.TempDir() // empty: no schemas at all

	v, err := New(Options{SchemaLocations: localLocation(dir)})
	if err != nil {
		t.Fatal(err)
	}
	results := v.Validate([]byte(`
apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: my-widget
spec:
  size: 42
`))

	if len(results) != 0 {
		t.Errorf("expected 0 results when no schema is available, got %d: %+v", len(results), results)
	}
}

func TestValidate_Strict(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "widget-test-v1.json", testWidgetSchema)

	// A duplicated YAML key is accepted by the lenient decoder and
	// rejected by the strict one.
	dupKey := `
apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: my-widget
spec:
  size: large
  size: small
  color: red
`

	v, err := New(Options{SchemaLocations: localLocation(dir)})
	if err != nil {
		t.Fatal(err)
	}
	if results := v.Validate([]byte(dupKey)); len(results) != 0 {
		t.Errorf("lenient mode must accept a duplicated key, got %d: %+v", len(results), results)
	}

	v, err = New(Options{SchemaLocations: localLocation(dir), Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	results := v.Validate([]byte(dupKey))
	if len(results) != 1 {
		t.Fatalf("strict mode must reject a duplicated key, got %d: %+v", len(results), results)
	}
	if !strings.Contains(strings.Join(results[0].Errors, " "), "size") {
		t.Errorf("expected the duplicated key in the error, got: %v", results[0].Errors)
	}
}

func TestValidate_SkipKinds(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "widget-test-v1.json", testWidgetSchema)

	// Missing spec.color: fails validation without skipping.
	invalidWidget := `
apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: my-widget
spec:
  size: large
`

	tests := []struct {
		name      string
		skipKinds []string
	}{
		{"plain kind matches any apiVersion", []string{"Widget"}},
		{"apiVersion/Kind matches exactly", []string{"test.example.com/v1/Widget"}},
		{"blank entries are ignored", []string{"", "Widget"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := New(Options{SchemaLocations: localLocation(dir), SkipKinds: tt.skipKinds})
			if err != nil {
				t.Fatal(err)
			}
			if results := v.Validate([]byte(invalidWidget)); len(results) != 0 {
				t.Errorf("expected the Widget to be skipped, got %d: %+v", len(results), results)
			}
		})
	}

	t.Run("other kinds do not match", func(t *testing.T) {
		v, err := New(Options{SchemaLocations: localLocation(dir), SkipKinds: []string{"Gadget", "other.example.com/v1/Widget"}})
		if err != nil {
			t.Fatal(err)
		}
		if results := v.Validate([]byte(invalidWidget)); len(results) != 1 {
			t.Errorf("expected the Widget to still fail, got %d: %+v", len(results), results)
		}
	})
}

func TestValidateAll_Statuses(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "widget-test-v1.json", testWidgetSchema)

	v, err := New(Options{SchemaLocations: localLocation(dir)})
	if err != nil {
		t.Fatal(err)
	}

	// One multi-doc stream with a valid Widget, an invalid one (missing
	// color), and a kind without a schema (skipped).
	results := v.ValidateAll([]byte(testWidgetResource + `---
apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: bad-widget
spec:
  size: large
---
apiVersion: test.example.com/v1
kind: Gadget
metadata:
  name: my-gadget
`))

	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d: %+v", len(results), results)
	}
	byName := make(map[string]Result, len(results))
	for _, r := range results {
		byName[r.Name] = r
	}
	if r := byName["my-widget"]; r.Status != StatusValid {
		t.Errorf("my-widget status = %q, want valid", r.Status)
	}
	if r := byName["my-gadget"]; r.Status != StatusSkipped {
		t.Errorf("my-gadget status = %q, want skipped", r.Status)
	}
	r := byName["bad-widget"]
	if r.Status != StatusInvalid {
		t.Errorf("bad-widget status = %q, want invalid", r.Status)
	}
	if len(r.Errors) == 0 {
		t.Error("bad-widget should carry validation errors")
	}
	if r.Kind != "Widget" || r.Namespace != "" || r.Label() != "Widget bad-widget" {
		t.Errorf("bad-widget identity fields = %+v, label = %q", r, r.Label())
	}

	if failures := Failures(results); len(failures) != 1 || failures[0].Name != "bad-widget" {
		t.Errorf("Failures() = %+v, want only bad-widget", failures)
	}
}

func TestValidate_EmptyData(t *testing.T) {
	v, err := New(Options{SchemaLocations: localLocation(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	if results := v.Validate([]byte("")); len(results) != 0 {
		t.Errorf("expected 0 results for empty data, got %d", len(results))
	}
}

func TestValidate_MalformedResource(t *testing.T) {
	v, err := New(Options{SchemaLocations: localLocation(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	// Not a Kubernetes resource: no apiVersion/kind. kubeconform reports it
	// as an error rather than skipping it silently.
	results := v.Validate([]byte("just: some\nyaml: data\n"))
	if len(results) != 1 {
		t.Fatalf("expected 1 result for a resource without apiVersion/kind, got %d: %+v", len(results), results)
	}
	if results[0].Label() != "malformed resource" {
		t.Errorf("resource label = %q, want malformed resource", results[0].Label())
	}
}

func TestValidate_CRDYAMLDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "test-crd.yaml", testCRDYAML)

	crdDir, err := CRDYAMLToSchemaDir(dir, "")
	if err != nil {
		t.Fatal(err)
	}

	v, err := New(Options{SchemaLocations: localLocation(crdDir)})
	if err != nil {
		t.Fatal(err)
	}

	results := v.Validate([]byte(`
apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: my-widget
spec:
  size: large
  color: red
`))
	if len(results) != 0 {
		t.Errorf("expected 0 validation errors from CRD YAML schema, got %d: %+v", len(results), results)
	}

	results = v.Validate([]byte(`
apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: my-widget
spec:
  size: large
`))
	if len(results) == 0 {
		t.Fatal("expected validation error for missing required field 'color' from CRD YAML schema")
	}
}

func TestCRDYAMLToSchemaDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "nested/crd.yaml", testCRDYAML)
	writeFile(t, dir, "ignored.txt", "not a CRD")

	out, err := CRDYAMLToSchemaDir(dir, "")
	if err != nil {
		t.Fatal(err)
	}

	// group test.example.com, version v1, kind Widget → widget-test-v1.json
	data, err := os.ReadFile(filepath.Join(out, "widget-test-v1.json"))
	if err != nil {
		t.Fatalf("expected converted schema widget-test-v1.json: %v", err)
	}
	if len(data) == 0 || data[0] != '{' {
		t.Errorf("converted schema is not JSON: %q", string(data[:min(len(data), 40)]))
	}

	entries, _ := os.ReadDir(out)
	if len(entries) != 1 {
		t.Errorf("expected exactly 1 converted schema, got %d", len(entries))
	}
}

func TestCRDYAMLToSchemaDir_NoCRDs(t *testing.T) {
	dir := t.TempDir()

	out, err := CRDYAMLToSchemaDir(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Errorf("expected empty result for a dir without CRDs, got %q", out)
	}
}

// TestCRDYAMLToSchemaDir_SkipsNestedGitRepos: a nested git repository under
// the schema dir (an external source clone cached inside the working tree)
// holds no CRD sources, and its template YAML would fail conversion as a
// hard error — the walk must not cross the repository boundary.
func TestCRDYAMLToSchemaDir_SkipsNestedGitRepos(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "nested/crd.yaml", testCRDYAML)

	clone := filepath.Join(dir, ".cache-fluxview", "git-sources", "data", "deadbeef")
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, clone, "template.yaml", "metadata:\n  name: x\n{{- if .Values.enabled }}\n  annotations:\n{{- end }}\n")

	out, err := CRDYAMLToSchemaDir(dir, "")
	if err != nil {
		t.Fatalf("nested git repo leaked into CRD conversion: %v", err)
	}

	entries, _ := os.ReadDir(out)
	if len(entries) != 1 {
		t.Errorf("expected exactly 1 converted schema, got %d", len(entries))
	}
}

// TestCRDYAMLToSchemaDir_ExoticSchemaConverts pins that valid CRDs using
// exotic-but-legal schema constructs convert fine — the conversion failure
// branches are hard errors, so a regression here must never fire on a
// legitimate CRD.
func TestCRDYAMLToSchemaDir_ExoticSchemaConverts(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "exotic-crd.yaml", `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: exotics.test.example.com
spec:
  group: test.example.com
  names:
    kind: Exotic
    plural: exotics
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            port:
              x-kubernetes-int-or-string: true
            payload:
              x-kubernetes-preserve-unknown-fields: true
              type: object
            size:
              type: string
              default: medium
              enum: [small, medium, large]
            template:
              type: object
              allOf:
                - required: [spec]
              properties:
                spec:
                  type: object
                  additionalProperties:
                    type: string
`)

	out, err := CRDYAMLToSchemaDir(dir, "")
	if err != nil {
		t.Fatalf("an exotic but valid CRD must convert, got: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(out, "exotic-test-v1.json"))
	if err != nil {
		t.Fatalf("expected converted schema exotic-test-v1.json: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("converted schema must be valid JSON: %v", err)
	}
	// int-or-string surfaces as the x-kubernetes extension on the property.
	props, _ := schema["properties"].(map[string]any)
	port, _ := props["port"].(map[string]any)
	if _, ok := port["x-kubernetes-int-or-string"]; !ok {
		t.Errorf("expected x-kubernetes-int-or-string on the port property, got: %s", data)
	}
}

// TestCRDYAMLToSchemaDir_BadRefFails pins the one conversion branch that
// is reachable today: spec.NewRef parses $ref as a URI, and a malformed
// value fails conversion. Under fail-open this silently dropped the kind;
// now it must fail the run — a schema with an unparseable $ref is a
// corrupt schema source (the API server's structural-schema validation
// would reject such a CRD too).
func TestCRDYAMLToSchemaDir_BadRefFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "badref-crd.yaml", `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: badrefs.example.com
spec:
  group: example.com
  names:
    kind: BadRef
    plural: badrefs
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
          properties:
            spec:
              $ref: "::"
`)

	out, err := CRDYAMLToSchemaDir(dir, "")
	if err == nil {
		t.Fatalf("a CRD with an unparseable $ref must fail the conversion, got dir %q", out)
	}
	if !strings.Contains(err.Error(), "could not convert CRD schema for example.com/v1 BadRef") {
		t.Errorf("error should name the kind whose schema failed to convert, got: %v", err)
	}
	if !strings.Contains(err.Error(), "missing protocol scheme") {
		t.Errorf("error should carry the URI parse failure, got: %v", err)
	}
}

func TestKubeconformSchemaName(t *testing.T) {
	tests := []struct {
		kind, group, version, want string
	}{
		{"HelmRelease", "helm.toolkit.fluxcd.io", "v2", "helmrelease-helm-v2.json"},
		{"Kustomization", "kustomize.toolkit.fluxcd.io", "v1", "kustomization-kustomize-v1.json"},
		{"Widget", "test.example.com", "v1beta1", "widget-test-v1beta1.json"},
		{"ClusterRole", "rbac.authorization.k8s.io", "v1", "clusterrole-rbac-v1.json"},
	}
	for _, tt := range tests {
		if got := kubeconformSchemaName(tt.kind, tt.group, tt.version); got != tt.want {
			t.Errorf("kubeconformSchemaName(%q, %q, %q) = %q, want %q", tt.kind, tt.group, tt.version, got, tt.want)
		}
	}
}

func TestDefaultKubernetesVersionIsSet(t *testing.T) {
	if DefaultKubernetesVersion == "" || !strings.HasPrefix(DefaultKubernetesVersion, "1.") {
		t.Errorf("DefaultKubernetesVersion = %q, want a plain 1.x.y version", DefaultKubernetesVersion)
	}
}

// --- Helpers ---

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// testWidgetSchema is a JSON Schema for the Widget test resource, named for
// the kubeconform local layout: group test.example.com, version v1.
const testWidgetSchema = `{
	"type": "object",
	"properties": {
		"spec": {"type": "object", "required": ["color"], "properties": {
			"size": {"type": "string", "enum": ["small", "medium", "large"]},
			"color": {"type": "string"}
		}}
	}
}`

// testWidgetResource is a valid instance of the Widget test resource.
const testWidgetResource = `apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: my-widget
spec:
  size: large
  color: red
`

// testCRDYAML is a minimal CRD definition for testing.
const testCRDYAML = `
apiVersion: apiextensions.k8s.io/v1
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
          required:
            - spec
          properties:
            spec:
              type: object
              required:
                - color
              properties:
                size:
                  type: string
                  enum: [small, medium, large]
                color:
                  type: string
`

// TestNew_EmptyLocations_NoDefault verifies the nil-vs-empty contract: an
// explicitly empty, non-nil location list means "no sources at all" and must
// not fall back to the default registry (which would need the network).
func TestNew_EmptyLocations_NoDefault(t *testing.T) {
	v, err := New(Options{SchemaLocations: []string{}})
	if err != nil {
		t.Fatal(err)
	}

	// With no sources every resource is skipped — including native kinds
	// the default registry would otherwise fetch over HTTP.
	results := v.Validate([]byte(testWidgetResource))
	if len(results) != 0 {
		t.Errorf("expected 0 results with no schema sources, got %d: %+v", len(results), results)
	}
	results = v.Validate([]byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\ndata:\n  k: 42\n"))
	if len(results) != 0 {
		t.Errorf("expected 0 results for ConfigMap with no schema sources, got %d: %+v", len(results), results)
	}
}

func TestNormalizeKubernetesVersion(t *testing.T) {
	tests := map[string]string{
		"1.36.1":   "1.36.1",
		"v1.36.1":  "1.36.1",
		" v1.36.1": "1.36.1",
		"":         "",
		"master":   "master",
	}
	for in, want := range tests {
		if got := NormalizeKubernetesVersion(in); got != want {
			t.Errorf("NormalizeKubernetesVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateKubernetesVersion(t *testing.T) {
	valid := []string{"", "1.36.1", "v1.36.1", " v1.34.0\t", "master", "1.19.0"}
	for _, in := range valid {
		if err := ValidateKubernetesVersion(in); err != nil {
			t.Errorf("ValidateKubernetesVersion(%q) = %v, want nil", in, err)
		}
	}

	// A short "1.36" 404s every default-registry schema and silently skips
	// all native kinds — the exact case the check exists for.
	invalid := []string{"1.36", "v1.36", "1", "latest", "1.x.y", "1.36.1.1", "master-1"}
	for _, in := range invalid {
		if err := ValidateKubernetesVersion(in); err == nil {
			t.Errorf("ValidateKubernetesVersion(%q) = nil, want an error", in)
		}
	}

	err := ValidateKubernetesVersion("1.36")
	if err == nil || !strings.Contains(err.Error(), "1.36.1") || !strings.Contains(err.Error(), "1.36") {
		t.Errorf("error should show the input and the expected form, got: %v", err)
	}
}
