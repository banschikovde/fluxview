package yamlutil

import (
	"strings"
	"testing"
)

// TestDedupDocs verifies that duplicate resources (same group/kind/namespace/name)
// are collapsed to one, last occurrence wins. Resources from different API groups
// with the same kind name are NOT collapsed.
func TestDedupDocs(t *testing.T) {
	input := []byte(`apiVersion: v1
kind: Namespace
metadata:
  name: podinfo
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: podinfo
data:
  key: first
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: podinfo
data:
  key: second
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: podinfo
  namespace: podinfo
`)
	result := DedupDocs(input)
	resultStr := string(result)

	docs := SplitYAMLText(result)
	if len(docs) != 3 {
		t.Errorf("expected 3 unique documents, got %d", len(docs))
	}

	// Last occurrence of ConfigMap should win.
	if strings.Contains(resultStr, "first") {
		t.Error("expected 'first' to be replaced by 'second' (last wins)")
	}
	if !strings.Contains(resultStr, "second") {
		t.Error("expected 'second' to be present (last occurrence wins)")
	}
}

// TestDedupDocs_DifferentGroups verifies that resources with the same
// kind/namespace/name but different API groups are NOT deduped.
func TestDedupDocs_DifferentGroups(t *testing.T) {
	input := []byte(`apiVersion: example.com/v1
kind: Endpoint
metadata:
  name: shared
  namespace: default
---
apiVersion: monitoring.coreos.com/v1
kind: Endpoint
metadata:
  name: shared
  namespace: default
`)
	resultStr := string(DedupDocs(input))

	// Both should survive — different API groups.
	if !strings.Contains(resultStr, "example.com/v1") {
		t.Error("example.com/v1 Endpoint should survive (different group)")
	}
	if !strings.Contains(resultStr, "monitoring.coreos.com/v1") {
		t.Error("monitoring.coreos.com/v1 Endpoint should survive (different group)")
	}
}

// TestDedupDocs_KeepsNonResources verifies that unparseable documents and
// documents without kind/metadata.name pass through untouched.
func TestDedupDocs_KeepsNonResources(t *testing.T) {
	input := []byte(`plain text: not a k8s resource
---
# comment-only document
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm
`)
	docs := SplitYAMLText(DedupDocs(input))
	if len(docs) != 3 {
		t.Errorf("expected all 3 documents kept, got %d", len(docs))
	}
}

// TestDedupDocs_Empty verifies empty input yields nil.
func TestDedupDocs_Empty(t *testing.T) {
	if got := DedupDocs(nil); got != nil {
		t.Errorf("DedupDocs(nil) = %q, want nil", got)
	}
}
