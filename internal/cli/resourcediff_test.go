package cli

import (
	"testing"

	"github.com/banschikovde/fluxview/internal/flux"
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

// TestBuildResourceMap verifies the single-pass processing of the diff states:
// namespace filtering (inlined here since metadata is already parsed), CRD
// skipping, attribute stripping, and secret redaction.
func TestBuildResourceMap(t *testing.T) {
	data := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: cm-a
  namespace: team-a
  creationTimestamp: "2024-01-01T00:00:00Z"
data:
  key: val
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm-b
  namespace: team-b
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: crds.example.com
---
apiVersion: v1
kind: Secret
metadata:
  name: sec
  namespace: team-a
type: Opaque
data:
  password: c2VjcmV0
`)
	key := func(kind, ns, name string) resourceKey {
		return resourceKey{Kind: kind, Namespace: ns, Name: name}
	}

	t.Run("no filters keeps all and redacts secrets", func(t *testing.T) {
		m := buildResourceMap(data, &DiffFlags{})
		if len(m) != 4 {
			t.Fatalf("expected 4 resources, got %d: %v", len(m), m)
		}
		if got := m[key("Secret", "team-a", "sec")]; !contains(got, flux.SecretRedactedValue) {
			t.Errorf("expected secret data redacted, got:\n%s", got)
		}
	})

	t.Run("namespace filter", func(t *testing.T) {
		m := buildResourceMap(data, &DiffFlags{Namespace: "team-a"})
		if len(m) != 2 {
			t.Fatalf("expected 2 team-a resources, got %d: %v", len(m), m)
		}
		if _, ok := m[key("ConfigMap", "team-b", "cm-b")]; ok {
			t.Error("team-b ConfigMap should be filtered out")
		}
	})

	t.Run("skip CRDs", func(t *testing.T) {
		m := buildResourceMap(data, &DiffFlags{SkipCRDs: true})
		if _, ok := m[key("CustomResourceDefinition", "", "crds.example.com")]; ok {
			t.Error("CRD should be skipped with SkipCRDs")
		}
		if len(m) != 3 {
			t.Errorf("expected 3 resources after CRD skip, got %d", len(m))
		}
	})

	t.Run("strip attrs", func(t *testing.T) {
		m := buildResourceMap(data, &DiffFlags{StripAttrs: "creationTimestamp"})
		got, ok := m[key("ConfigMap", "team-a", "cm-a")]
		if !ok {
			t.Fatal("expected cm-a to be present")
		}
		if contains(got, "creationTimestamp") {
			t.Errorf("expected creationTimestamp stripped, got:\n%s", got)
		}
	})

	t.Run("duplicate key overwrites", func(t *testing.T) {
		dup := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: cm-dup
data:
  key: first
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm-dup
data:
  key: second
`)
		stderr := captureStderr(func() {
			m := buildResourceMap(dup, &DiffFlags{})
			if got := m[key("ConfigMap", "", "cm-dup")]; !contains(got, "second") {
				t.Errorf("expected last duplicate to win, got:\n%s", got)
			}
		})
		if !contains(stderr, "duplicate resource") {
			t.Errorf("expected a duplicate-resource warning, got:\n%s", stderr)
		}
	})
}
