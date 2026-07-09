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
