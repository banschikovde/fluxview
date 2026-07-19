package cli

import (
	"testing"
)

func TestParseAttrs(t *testing.T) {
	tests := []struct {
		input string
		want  map[string]bool
	}{
		{"", nil},
		{"status", map[string]bool{"status": true}},
		{"helm.sh/chart,app.kubernetes.io/version", map[string]bool{"helm.sh/chart": true, "app.kubernetes.io/version": true}},
		{" a , b , c ", map[string]bool{"a": true, "b": true, "c": true}},
		{"status,,creationTimestamp", map[string]bool{"status": true, "creationTimestamp": true}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseAttrs(tt.input)
			if len(got) != len(tt.want) {
				t.Errorf("parseAttrs(%q) = %v, want %v", tt.input, got, tt.want)
				return
			}
			for k := range tt.want {
				if !got[k] {
					t.Errorf("parseAttrs(%q) missing key %q", tt.input, k)
				}
			}
		})
	}
}

func TestStripAttrsFromDoc(t *testing.T) {
	doc := `apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  creationTimestamp: "2024-01-01T00:00:00Z"
  annotations:
    helm.sh/chart: nginx-1.0.0
    app.kubernetes.io/name: nginx
spec:
  replicas: 1
status:
  readyReplicas: 1
`
	attrs := map[string]bool{"creationTimestamp": true, "helm.sh/chart": true, "status": true}

	result := stripAttrsFromDoc(doc, attrs)

	if contains(result, "creationTimestamp") {
		t.Error("expected creationTimestamp to be stripped")
	}
	if contains(result, "helm.sh/chart") {
		t.Error("expected helm.sh/chart to be stripped")
	}
	if contains(result, "readyReplicas") {
		t.Error("expected status to be stripped (readyReplicas gone)")
	}
	if !contains(result, "app.kubernetes.io/name") {
		t.Error("expected app.kubernetes.io/name to be kept")
	}
	if !contains(result, "replicas: 1") {
		t.Error("expected replicas to be kept")
	}
}

func TestStripAttrsFromDoc_EmptyAttrs(t *testing.T) {
	doc := `kind: ConfigMap
metadata:
  name: test
`
	result := stripAttrsFromDoc(doc, nil)
	// With empty attrs, doc is returned as-is.
	if result != doc {
		t.Errorf("expected doc unchanged, got %q", result)
	}
}

func TestStripAllAttrs(t *testing.T) {
	data := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: cm1
  creationTimestamp: "2024-01-01"
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm2
  creationTimestamp: "2024-01-02"
`)
	result := stripAllAttrs(data, "creationTimestamp")
	if contains(string(result), "creationTimestamp") {
		t.Error("expected creationTimestamp to be stripped from all docs")
	}
	if !contains(string(result), "name: cm1") {
		t.Error("expected cm1 to be present")
	}
	if !contains(string(result), "name: cm2") {
		t.Error("expected cm2 to be present")
	}
}

func TestFilterCRDDocs_KeepsUnparseable(t *testing.T) {
	data := []byte(`apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.com
---
::: broken yaml :::
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: kept
`)
	result := filterCRDDocs(data)
	resultStr := string(result)
	if !contains(resultStr, "name: kept") {
		t.Error("expected ConfigMap to be kept")
	}
	if contains(resultStr, "CustomResourceDefinition") {
		t.Error("expected CRD to be filtered out")
	}
	if !contains(resultStr, "broken yaml") {
		t.Error("expected unparseable doc to be kept (conservative behavior)")
	}
}

func TestInjectNamespace_AddsToNamespacedKind(t *testing.T) {
	data := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
    name: app
spec:
    replicas: 1
`)
	got := string(injectNamespace(data, "my-ns"))
	if !contains(got, "name: app") {
		t.Errorf("expected name preserved:\n%s", got)
	}
	if !contains(got, "namespace: my-ns") {
		t.Errorf("expected namespace injected:\n%s", got)
	}
	// Verify namespace lands immediately after name (k8s convention).
	// yaml.Node re-encodes with 4-space indent to match Helm's output
	// (ConvertJSONInYAMLToYAML uses yaml.Marshal default).
	if !contains(got, "metadata:\n    name: app\n    namespace: my-ns") {
		t.Errorf("expected namespace immediately after name:\n%s", got)
	}
}

func TestInjectNamespace_PreservesExistingNamespace(t *testing.T) {
	data := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
    name: app
    namespace: existing
spec:
    replicas: 1
`)
	got := string(injectNamespace(data, "my-ns"))
	if contains(got, "namespace: my-ns") {
		t.Errorf("must not override existing namespace:\n%s", got)
	}
	if !contains(got, "namespace: existing") {
		t.Errorf("existing namespace must remain:\n%s", got)
	}
}

func TestInjectNamespace_SkipsClusterScopedKinds(t *testing.T) {
	data := []byte(`apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.com
spec:
  group: example.com
`)
	got := string(injectNamespace(data, "my-ns"))
	if contains(got, "namespace: my-ns") {
		t.Errorf("CRD must not get namespace:\n%s", got)
	}
	// Same for Namespace, ClusterRole, etc.
	for _, kind := range []string{"Namespace", "ClusterRole", "StorageClass"} {
		doc := "apiVersion: v1\nkind: " + kind + "\nmetadata:\n  name: x\n"
		got := string(injectNamespace([]byte(doc), "my-ns"))
		if contains(got, "namespace:") {
			t.Errorf("%s must not get namespace:\n%s", kind, got)
		}
	}
}

func TestInjectNamespace_SkipsNonResources(t *testing.T) {
	// No apiVersion/kind → not a k8s resource, leave as-is.
	data := []byte(`foo: bar
baz: qux
`)
	got := string(injectNamespace(data, "my-ns"))
	// We don't require byte-equality (yaml.Node may re-encode trailing
	// whitespace), only that no namespace was injected.
	if contains(got, "namespace:") {
		t.Errorf("non-resource doc must not get namespace injected:\n%s", got)
	}
}

func TestInjectNamespace_EmptyNamespaceNoOp(t *testing.T) {
	data := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: cm
`)
	// Empty namespace → return data unchanged (no scan).
	got := injectNamespace(data, "")
	if string(got) != string(data) {
		t.Errorf("empty namespace must be no-op")
	}
}

func TestInjectNamespace_MultiDoc(t *testing.T) {
	data := []byte(`apiVersion: apps/v1
kind: Deployment
metadata:
    name: app
---
apiVersion: v1
kind: ConfigMap
metadata:
    name: cm
    namespace: preset
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
    name: widgets.example.com
`)
	got := string(injectNamespace(data, "injected"))

	// Deployment: namespace added (4-space indent matches Helm output).
	if !contains(got, "kind: Deployment\nmetadata:\n    name: app\n    namespace: injected") {
		t.Errorf("Deployment should get namespace injected:\n%s", got)
	}
	// ConfigMap: existing namespace preserved.
	if !contains(got, "name: cm\n    namespace: preset") {
		t.Errorf("ConfigMap existing namespace should be preserved:\n%s", got)
	}
	// CRD: cluster-scoped, no namespace. Find the CRD block and verify it
	// has no `namespace:` line inside its metadata.
	crdIdx := indexOfFrom(got, "widgets.example.com", 0)
	if crdIdx == -1 {
		t.Fatalf("CRD name missing from output:\n%s", got)
	}
	end := crdIdx + 200
	if end > len(got) {
		end = len(got)
	}
	if contains(got[crdIdx:end], "namespace:") {
		t.Errorf("CRD should not get namespace:\n%s", got[crdIdx:end])
	}
}

// indexOfFrom returns the index of sub in s starting from offset, or -1.
func indexOfFrom(s, sub string, from int) int {
	if from < 0 {
		from = 0
	}
	if from >= len(s) {
		return -1
	}
	for i := from; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestInjectNamespace_MetadataWithoutName(t *testing.T) {
	// A resource with metadata but no name (rare, but possible). Namespace
	// should be inserted at the start of the metadata mapping.
	data := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  labels:
    app: foo
`)
	got := string(injectNamespace(data, "my-ns"))
	if !contains(got, "namespace: my-ns") {
		t.Errorf("namespace should be injected even without name:\n%s", got)
	}
}

func TestInjectNamespace_PreservesScalarStyles(t *testing.T) {
	// Quoted strings, block scalars, comments — yaml.Node must preserve them.
	data := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: "quoted-name"  # important comment
data:
  values.yaml: |
    multi: line
    block: scalar
`)
	got := string(injectNamespace(data, "my-ns"))
	if !contains(got, `"quoted-name"`) {
		t.Errorf("quoted scalar must be preserved:\n%s", got)
	}
	if !contains(got, "# important comment") {
		t.Errorf("comment must be preserved:\n%s", got)
	}
	if !contains(got, "multi: line") || !contains(got, "block: scalar") {
		t.Errorf("block scalar must be preserved:\n%s", got)
	}
	if !contains(got, "namespace: my-ns") {
		t.Errorf("namespace must be injected:\n%s", got)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
