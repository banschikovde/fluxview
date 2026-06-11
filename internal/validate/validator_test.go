package validate

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestNew_EmptyDir(t *testing.T) {
	v := New("")
	if v.SchemaCount() != 0 {
		t.Errorf("expected 0 schemas, got %d", v.SchemaCount())
	}
}

func TestNew_NonExistentDir(t *testing.T) {
	v := New("/nonexistent/path")
	if v.SchemaCount() != 0 {
		t.Errorf("expected 0 schemas, got %d", v.SchemaCount())
	}
}

func TestNew_CRDFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "test-crd.yaml", testCRDYAML)

	v := New(dir)
	if v.SchemaCount() != 1 {
		t.Fatalf("expected 1 schema, got %d", v.SchemaCount())
	}
	gk := schema.GroupKind{Group: "test.example.com", Kind: "Widget"}
	if _, ok := v.schemas[gk]; !ok {
		t.Error("expected schema for test.example.com/Widget")
	}
}

func TestNew_CRDFileRecursive(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "custom")
	os.MkdirAll(sub, 0755)
	writeFile(t, sub, "nested-crd.yaml", testCRDYAML)

	v := New(dir)
	if v.SchemaCount() != 1 {
		t.Fatalf("expected 1 schema from subdirectory, got %d", v.SchemaCount())
	}
}

func TestValidate_ValidResource(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "test-crd.yaml", testCRDYAML)

	v := New(dir)
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

func TestValidate_InvalidResource_MissingRequired(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "test-crd.yaml", testCRDYAML)

	v := New(dir)
	results := v.Validate([]byte(`
apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: my-widget
spec:
  size: large
`))

	if len(results) == 0 {
		t.Fatal("expected validation error for missing required field 'color'")
	}
}

func TestValidate_InvalidResource_EnumViolation(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "test-crd.yaml", testCRDYAML)

	v := New(dir)
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

func TestValidate_NoSchema_SkipsSilently(t *testing.T) {
	v := New("")
	results := v.Validate([]byte(`
apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: my-widget
spec:
  size: 42
`))

	if len(results) != 0 {
		t.Errorf("expected 0 results when no schema loaded, got %d", len(results))
	}
}

func TestValidate_EmptyData(t *testing.T) {
	v := New("")
	results := v.Validate([]byte(""))
	if len(results) != 0 {
		t.Errorf("expected 0 results for empty data, got %d", len(results))
	}
}

func TestFixRefs(t *testing.T) {
	m := map[string]interface{}{
		"$ref": "_definitions.json#/definitions/Foo",
		"nested": map[string]interface{}{
			"$ref": "_definitions.json#/definitions/Bar",
		},
		"items": []interface{}{
			map[string]interface{}{
				"$ref": "_definitions.json#/definitions/Baz",
			},
		},
	}

	fixRefs(m)

	if m["$ref"] != "#/definitions/Foo" {
		t.Errorf("top-level $ref = %v, want #/definitions/Foo", m["$ref"])
	}
	nested := m["nested"].(map[string]interface{})
	if nested["$ref"] != "#/definitions/Bar" {
		t.Errorf("nested $ref = %v, want #/definitions/Bar", nested["$ref"])
	}
	items := m["items"].([]interface{})
	item := items[0].(map[string]interface{})
	if item["$ref"] != "#/definitions/Baz" {
		t.Errorf("array item $ref = %v, want #/definitions/Baz", item["$ref"])
	}
}

func TestExtractMeta(t *testing.T) {
	obj := map[string]interface{}{
		"apiVersion": "test.example.com/v1",
		"kind":       "Widget",
		"metadata": map[string]interface{}{
			"name":      "my-widget",
			"namespace": "default",
		},
	}

	meta, ok := extractMeta(obj)
	if !ok {
		t.Fatal("expected ok")
	}
	if meta.Kind != "Widget" {
		t.Errorf("kind = %q, want Widget", meta.Kind)
	}
	if meta.Name != "my-widget" {
		t.Errorf("name = %q, want my-widget", meta.Name)
	}
}

func TestExtractMeta_MissingFields(t *testing.T) {
	_, ok := extractMeta(map[string]interface{}{"kind": "Widget"})
	if ok {
		t.Error("expected false when apiVersion is missing")
	}
}

func TestParseAPIVersion(t *testing.T) {
	tests := []struct {
		input      string
		group, ver string
	}{
		{"test.example.com/v1", "test.example.com", "v1"},
		{"v1", "", "v1"},
		{"helm.toolkit.fluxcd.io/v2beta1", "helm.toolkit.fluxcd.io", "v2beta1"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			g, v := parseAPIVersion(tt.input)
			if g != tt.group || v != tt.ver {
				t.Errorf("parseAPIVersion(%q) = (%q, %q), want (%q, %q)", tt.input, g, v, tt.group, tt.ver)
			}
		})
	}
}

func TestSplitYAMLDocs(t *testing.T) {
	data := []byte("doc1: val1\n---\ndoc2: val2\n---\n\ndoc3: val3")
	docs := splitYAMLDocs(data)
	if len(docs) != 3 {
		t.Fatalf("expected 3 docs, got %d", len(docs))
	}
}

func TestFluxSchemaKinds(t *testing.T) {
	gk, ok := fluxSchemaKinds["helmrelease-helm-v2"]
	if !ok {
		t.Fatal("expected helmrelease-helm-v2 in fluxSchemaKinds")
	}
	if gk.Kind != "HelmRelease" {
		t.Errorf("kind = %q, want HelmRelease", gk.Kind)
	}
	if gk.Group != "helm.toolkit.fluxcd.io" {
		t.Errorf("group = %q, want helm.toolkit.fluxcd.io", gk.Group)
	}
}

// --- Helpers ---

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

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
