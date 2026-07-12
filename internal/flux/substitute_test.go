package flux

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

func TestApplySubstitution(t *testing.T) {
	tests := []struct {
		name string
		data string
		vars map[string]string
		want string
	}{
		{
			name: "dollar-brace pattern",
			data: "image: registry/${CLUSTER_NAME}/app:v1",
			vars: map[string]string{"CLUSTER_NAME": "prod"},
			want: "image: registry/prod/app:v1",
		},
		{
			name: "dollar-paren pattern",
			data: "image: registry/$(CLUSTER_NAME)/app:v1",
			vars: map[string]string{"CLUSTER_NAME": "prod"},
			want: "image: registry/prod/app:v1",
		},
		{
			name: "multiple variables",
			data: "host: ${APP}.${DOMAIN}",
			vars: map[string]string{"APP": "api", "DOMAIN": "example.com"},
			want: "host: api.example.com",
		},
		{
			name: "no variables",
			data: "key: value",
			vars: map[string]string{"VAR": "x"},
			want: "key: value",
		},
		{
			name: "empty vars",
			data: "key: ${VAR}",
			vars: nil,
			want: "key: ${VAR}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(ApplySubstitution([]byte(tt.data), tt.vars))
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveSubstituteVars(t *testing.T) {
	ks := Kustomization{
		Metadata: struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		}{Name: "base", Namespace: "flux-system"},
		Spec: KustomizationSpec{
			PostBuild: &PostBuild{
				SubstituteFrom: []interface{}{
					map[string]interface{}{
						"kind": "ConfigMap",
						"name": "cluster-settings",
					},
				},
			},
		},
	}

	cms := []ConfigMap{
		{
			Metadata: struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			}{Name: "cluster-settings", Namespace: "flux-system"},
			Data: map[string]string{
				"CLUSTER_NAME": "prod-us-east",
				"DOMAIN":       "example.com",
			},
		},
	}

	vars := ResolveSubstituteVars(ks, cms)
	if len(vars) != 2 {
		t.Fatalf("expected 2 vars, got %d", len(vars))
	}
	if vars["CLUSTER_NAME"] != "prod-us-east" {
		t.Errorf("CLUSTER_NAME = %q, want %q", vars["CLUSTER_NAME"], "prod-us-east")
	}
	if vars["DOMAIN"] != "example.com" {
		t.Errorf("DOMAIN = %q, want %q", vars["DOMAIN"], "example.com")
	}
}

func TestResolveSubstituteVars_InlineOverrides(t *testing.T) {
	ks := Kustomization{
		Metadata: struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		}{Name: "base", Namespace: "flux-system"},
		Spec: KustomizationSpec{
			PostBuild: &PostBuild{
				Substitute: map[string]string{"CLUSTER_NAME": "override"},
				SubstituteFrom: []interface{}{
					map[string]interface{}{
						"kind": "ConfigMap",
						"name": "cluster-settings",
					},
				},
			},
		},
	}

	cms := []ConfigMap{
		{
			Metadata: struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			}{Name: "cluster-settings", Namespace: "flux-system"},
			Data: map[string]string{"CLUSTER_NAME": "from-cm"},
		},
	}

	vars := ResolveSubstituteVars(ks, cms)
	if vars["CLUSTER_NAME"] != "override" {
		t.Errorf("CLUSTER_NAME = %q, want %q (inline should override)", vars["CLUSTER_NAME"], "override")
	}
}

func TestTopologicalSort(t *testing.T) {
	ks := []Kustomization{
		{
			Metadata: struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			}{Name: "apps", Namespace: "flux-system"},
			Spec: KustomizationSpec{
				DependsOn: []DependsOnEntry{{Name: "base"}},
			},
		},
		{
			Metadata: struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			}{Name: "base", Namespace: "flux-system"},
			Spec: KustomizationSpec{},
		},
	}

	sorted, err := TopologicalSort(ks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sorted) != 2 {
		t.Fatalf("expected 2 items, got %d", len(sorted))
	}
	if sorted[0].Metadata.Name != "base" {
		t.Errorf("first item = %q, want %q (dependency should come first)", sorted[0].Metadata.Name, "base")
	}
	if sorted[1].Metadata.Name != "apps" {
		t.Errorf("second item = %q, want %q", sorted[1].Metadata.Name, "apps")
	}
}

func TestTopologicalSort_Circular(t *testing.T) {
	ks := []Kustomization{
		{
			Metadata: struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			}{Name: "a", Namespace: "flux-system"},
			Spec: KustomizationSpec{
				DependsOn: []DependsOnEntry{{Name: "b"}},
			},
		},
		{
			Metadata: struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			}{Name: "b", Namespace: "flux-system"},
			Spec: KustomizationSpec{
				DependsOn: []DependsOnEntry{{Name: "a"}},
			},
		},
	}

	_, err := TopologicalSort(ks)
	if err == nil {
		t.Fatal("expected error for circular dependency")
	}
}

func TestParseSubstituteFrom(t *testing.T) {
	raw := []interface{}{
		map[string]interface{}{
			"kind": "ConfigMap",
			"name": "cluster-settings",
		},
		map[string]interface{}{
			"kind": "Secret",
			"name": "cluster-secrets",
		},
	}

	entries := parseSubstituteFrom(raw)
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Kind != "ConfigMap" || entries[0].Name != "cluster-settings" {
		t.Errorf("entry[0] = %+v, unexpected", entries[0])
	}
	if entries[1].Kind != "Secret" || entries[1].Name != "cluster-secrets" {
		t.Errorf("entry[1] = %+v, unexpected", entries[1])
	}
}

func TestSubstituteNeeded(t *testing.T) {
	tests := []struct {
		name string
		ks   Kustomization
		want bool
	}{
		{
			name: "no postBuild",
			ks:   Kustomization{Spec: KustomizationSpec{}},
			want: false,
		},
		{
			name: "disabled",
			ks:   Kustomization{Spec: KustomizationSpec{PostBuild: &PostBuild{DisableSubstitute: true}}},
			want: false,
		},
		{
			name: "has substitute",
			ks:   Kustomization{Spec: KustomizationSpec{PostBuild: &PostBuild{Substitute: map[string]string{"A": "B"}}}},
			want: true,
		},
		{
			name: "has substituteFrom",
			ks:   Kustomization{Spec: KustomizationSpec{PostBuild: &PostBuild{SubstituteFrom: []interface{}{map[string]interface{}{"kind": "ConfigMap"}}}}},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SubstituteNeeded(tt.ks)
			if got != tt.want {
				t.Errorf("SubstituteNeeded() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplySubstitution_Defaults(t *testing.T) {
	vars := map[string]string{"SET": "value", "EMPTY": ""}

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"simple resolved", "host: ${SET}", "host: value"},
		{"unresolved becomes empty (Flux)", "host: ${UNSET}", "host: "},
		{"assign default var set", "${SET:=def}", "value"},
		{"assign default var unset", "${UNSET:=def}", "def"},
		{"dash default var set", "${SET:-def}", "value"},
		{"dash default var empty", "${EMPTY:-def}", "def"},
		{"dash default var unset", "${UNSET:-def}", "def"},
		{"assign default var empty", "${EMPTY:=def}", "def"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := string(ApplySubstitution([]byte(tt.input), vars))
			if result != tt.want {
				t.Errorf("ApplySubstitution(%q) = %q, want %q", tt.input, result, tt.want)
			}
		})
	}
}

func TestTopologicalSortHelmReleases(t *testing.T) {
	hr := []HelmRelease{
		{
			Metadata: ObjectMeta{Name: "app", Namespace: "flux-system"},
			Spec: HelmReleaseSpec{
				DependsOn: []DependsOnEntry{{Name: "db"}},
			},
		},
		{
			Metadata: ObjectMeta{Name: "db", Namespace: "flux-system"},
			Spec:     HelmReleaseSpec{},
		},
	}

	sorted, err := TopologicalSortHelmReleases(hr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sorted) != 2 {
		t.Fatalf("expected 2 items, got %d", len(sorted))
	}
	if sorted[0].Metadata.Name != "db" {
		t.Errorf("first item = %q, want %q (dependency should come first)", sorted[0].Metadata.Name, "db")
	}
	if sorted[1].Metadata.Name != "app" {
		t.Errorf("second item = %q, want %q", sorted[1].Metadata.Name, "app")
	}
}

func TestTopologicalSortHelmReleases_Circular(t *testing.T) {
	hr := []HelmRelease{
		{
			Metadata: ObjectMeta{Name: "a", Namespace: "flux-system"},
			Spec: HelmReleaseSpec{
				DependsOn: []DependsOnEntry{{Name: "b"}},
			},
		},
		{
			Metadata: ObjectMeta{Name: "b", Namespace: "flux-system"},
			Spec: HelmReleaseSpec{
				DependsOn: []DependsOnEntry{{Name: "a"}},
			},
		},
	}

	_, err := TopologicalSortHelmReleases(hr)
	if err == nil {
		t.Fatal("expected error for circular dependency")
	}
}

func TestTopologicalSortHelmReleases_Namespace(t *testing.T) {
	hr := []HelmRelease{
		{
			Metadata: ObjectMeta{Name: "app", Namespace: "app-ns"},
			Spec: HelmReleaseSpec{
				DependsOn: []DependsOnEntry{{Name: "db", Namespace: "db-ns"}},
			},
		},
		{
			Metadata: ObjectMeta{Name: "db", Namespace: "db-ns"},
			Spec:     HelmReleaseSpec{},
		},
	}

	sorted, err := TopologicalSortHelmReleases(hr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(sorted) != 2 {
		t.Fatalf("expected 2 items, got %d", len(sorted))
	}
	if sorted[0].Metadata.Name != "db" || sorted[0].Metadata.Namespace != "db-ns" {
		t.Errorf("first item = %q, want %q (dependency should come first)", sorted[0].Metadata.Name, "db")
	}
	if sorted[1].Metadata.Name != "app" || sorted[1].Metadata.Namespace != "app-ns" {
		t.Errorf("second item = %q, want %q", sorted[1].Metadata.Name, "app")
	}
}

func TestResolveValuesFrom(t *testing.T) {
	hr := HelmRelease{
		Metadata: ObjectMeta{Name: "app", Namespace: "flux-system"},
		Spec: HelmReleaseSpec{
			ValuesFrom: []interface{}{
				map[string]interface{}{
					"kind": "ConfigMap",
					"name": "app-config",
				},
			},
		},
	}

	configMaps := []ConfigMap{
		{
			Metadata: ObjectMeta{Name: "app-config", Namespace: "flux-system"},
			Data: map[string]string{
				"values.yaml": "key1: value1\nkey2: value2\n",
			},
		},
	}

	result := ResolveValuesFrom(hr, configMaps, nil)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result["key1"] != "value1" {
		t.Errorf("key1 = %q, want %q", result["key1"], "value1")
	}
	if result["key2"] != "value2" {
		t.Errorf("key2 = %q, want %q", result["key2"], "value2")
	}
}

// TestResolveValuesFrom_ValuesKey verifies that valuesKey selects a specific
// key from ConfigMap data and parses it as YAML (Flux behavior).
func TestResolveValuesFrom_ValuesKey(t *testing.T) {
	hr := HelmRelease{
		Metadata: ObjectMeta{Name: "app", Namespace: "podinfo"},
		Spec: HelmReleaseSpec{
			ValuesFrom: []interface{}{
				map[string]interface{}{
					"kind":      "ConfigMap",
					"name":      "app-values",
					"valuesKey": "values.yaml",
				},
			},
		},
	}

	configMaps := []ConfigMap{
		{
			Metadata: ObjectMeta{Name: "app-values", Namespace: "podinfo"},
			Data: map[string]string{
				"values.yaml": "ui:\n  color: \"#114463\"\n  message: hello\n",
				"other-key":   "should-be-ignored",
			},
		},
	}

	result := ResolveValuesFrom(hr, configMaps, nil)
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	// valuesKey content should be parsed as YAML and merged.
	ui, ok := result["ui"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected 'ui' map in result, got %T for 'ui'", result["ui"])
	}
	if ui["color"] != "#114463" {
		t.Errorf("ui.color = %v, want #114463", ui["color"])
	}
	if ui["message"] != "hello" {
		t.Errorf("ui.message = %v, want hello", ui["message"])
	}

	// Keys NOT referenced by valuesKey should NOT be in result.
	if _, exists := result["other-key"]; exists {
		t.Error("non-valuesKey data should not be in result")
	}
	if _, exists := result["values.yaml"]; exists {
		t.Error("values.yaml key itself should not be in result (it's the valuesKey selector)")
	}
}

// TestResolveValuesFrom_MultiEntryConfigMap verifies that multiple valuesFrom
// entries are resolved independently. Both entries must have exact namespace match.
func TestResolveValuesFrom_MultiEntryConfigMap(t *testing.T) {
	hr := HelmRelease{
		Metadata: ObjectMeta{Name: "app", Namespace: "podinfo"},
		Spec: HelmReleaseSpec{
			ValuesFrom: []interface{}{
				map[string]interface{}{
					"kind": "ConfigMap",
					"name": "cm1",
				},
				map[string]interface{}{
					"kind": "ConfigMap",
					"name": "cm2",
				},
			},
		},
	}

	configMaps := []ConfigMap{
		{
			Metadata: ObjectMeta{Name: "cm1", Namespace: "podinfo"},
			Data:     map[string]string{"values.yaml": "key1: val1\n"},
		},
		{
			Metadata: ObjectMeta{Name: "cm2", Namespace: "podinfo"},
			Data:     map[string]string{"values.yaml": "key2: val2\n"},
		},
	}

	result := ResolveValuesFrom(hr, configMaps, nil)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result["key1"] != "val1" {
		t.Errorf("key1 = %v, want val1", result["key1"])
	}
	if result["key2"] != "val2" {
		t.Errorf("key2 = %v, want val2", result["key2"])
	}
}

// TestResolveValuesFrom_DifferentNamespaceIgnored verifies that a resource
// with an explicit different namespace is NOT matched — only exact match works.
func TestResolveValuesFrom_DifferentNamespaceIgnored(t *testing.T) {
	hr := HelmRelease{
		Metadata: ObjectMeta{Name: "app", Namespace: "test"},
		Spec: HelmReleaseSpec{
			ValuesFrom: []interface{}{
				map[string]interface{}{
					"kind": "ConfigMap",
					"name": "shared-cm",
				},
			},
		},
	}

	// Only a staging version exists — should NOT match.
	configMaps := []ConfigMap{
		{
			Metadata: ObjectMeta{Name: "shared-cm", Namespace: "staging"},
			Data:     map[string]string{"values.yaml": "from-staging: true\n"},
		},
	}

	result := ResolveValuesFrom(hr, configMaps, nil)
	if len(result) != 0 {
		t.Errorf("expected empty result — different namespace should not match, got %v", result)
	}
}

// TestResolveValuesFrom_NotFoundWarning verifies that when no matching resource
// is found (neither exact nor fallback), a warning is printed to stderr.
func TestResolveValuesFrom_NotFoundWarning(t *testing.T) {
	hr := HelmRelease{
		Metadata: ObjectMeta{Name: "app", Namespace: "test"},
		Spec: HelmReleaseSpec{
			ValuesFrom: []interface{}{
				map[string]interface{}{
					"kind": "ConfigMap",
					"name": "nonexistent",
				},
			},
		},
	}

	// Capture stderr.
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	result := ResolveValuesFrom(hr, nil, nil)

	w.Close()
	os.Stderr = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	stderrOutput := buf.String()

	// result should be non-nil but empty (entry was skipped).
	if len(result) != 0 {
		t.Errorf("expected empty result for nonexistent ConfigMap, got %v", result)
	}
	// Warning must be printed to stderr.
	if !strings.Contains(stderrOutput, "Warning:") {
		t.Errorf("expected warning in stderr, got:\n%s", stderrOutput)
	}
	if !strings.Contains(stderrOutput, "nonexistent") {
		t.Errorf("expected ConfigMap name 'nonexistent' in warning, got:\n%s", stderrOutput)
	}
}

// TestResolveValuesFrom_SecretsNotResolved verifies that real secret values
// are NOT injected into Helm rendering — instead, YAML-safe placeholder values
// are used so the chart renders but secrets never leak.
// Tests with base64-encoded Data (the standard Kubernetes Secret format).
func TestResolveValuesFrom_SecretsNotResolved(t *testing.T) {
	// "password: super-secret-password\ntoken: abc123\n" base64-encoded.
	encoded := "cGFzc3dvcmQ6IHN1cGVyLXNlY3JldC1wYXNzd29yZAp0b2tlbjogYWJjMTIzCg=="

	hr := HelmRelease{
		Metadata: ObjectMeta{Name: "app", Namespace: "flux-system"},
		Spec: HelmReleaseSpec{
			ValuesFrom: []interface{}{
				map[string]interface{}{
					"kind": "Secret",
					"name": "app-secrets",
				},
			},
		},
	}

	secrets := []Secret{
		{
			Metadata: ObjectMeta{Name: "app-secrets", Namespace: "flux-system"},
			Data: map[string]string{
				"values.yaml": encoded, // base64-encoded
			},
		},
	}

	result := ResolveValuesFrom(hr, nil, secrets)

	if result == nil {
		t.Fatal("expected non-nil result — secret placeholders should be injected")
	}
	if _, ok := result["password"]; !ok {
		t.Error("secret key 'password' missing from result")
	}
	// Real values must NEVER appear.
	if result["password"] == "super-secret-password" {
		t.Error("real secret value leaked into Helm values")
	}
	if result["password"] != SecretHelmPlaceholder {
		t.Errorf("expected %q placeholder, got %v", SecretHelmPlaceholder, result["password"])
	}
	if result["token"] != SecretHelmPlaceholder {
		t.Errorf("expected %q placeholder for token, got %v", SecretHelmPlaceholder, result["token"])
	}
}

// TestResolveValuesFrom_SecretStringData verifies that stringData (plaintext)
// secret values are also handled correctly.
func TestResolveValuesFrom_SecretStringData(t *testing.T) {
	hr := HelmRelease{
		Metadata: ObjectMeta{Name: "app", Namespace: "podinfo"},
		Spec: HelmReleaseSpec{
			ValuesFrom: []interface{}{
				map[string]interface{}{
					"kind":      "Secret",
					"name":      "app-secrets",
					"valuesKey": "values.yaml",
				},
			},
		},
	}

	secrets := []Secret{
		{
			Metadata: ObjectMeta{Name: "app-secrets", Namespace: "podinfo"},
			StringData: map[string]string{
				"values.yaml": "backend: \"http://example.com\"\n",
			},
		},
	}

	result := ResolveValuesFrom(hr, nil, secrets)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result["backend"] != SecretHelmPlaceholder {
		t.Errorf("expected %q placeholder, got %v", SecretHelmPlaceholder, result["backend"])
	}
}

// TestResolveValuesFrom_SecretNoNamespaceNoMatch verifies that a Secret
// without namespace does NOT match when valuesFrom explicitly specifies
// a namespace (prevents stale cross-namespace match).
func TestResolveValuesFrom_SecretNoNamespaceNoMatch(t *testing.T) {
	hr := HelmRelease{
		Metadata: ObjectMeta{Name: "app", Namespace: "podinfo"},
		Spec: HelmReleaseSpec{
			ValuesFrom: []interface{}{
				map[string]interface{}{
					"kind":      "Secret",
					"name":      "app-secrets",
					"namespace": "test", // explicit namespace
				},
			},
		},
	}

	// Secret with no namespace (raw-parsed from unrelated directory).
	secrets := []Secret{
		{
			Metadata: ObjectMeta{Name: "app-secrets"}, // empty namespace
			StringData: map[string]string{
				"values.yaml": "key: value\n",
			},
		},
	}

	result := ResolveValuesFrom(hr, nil, secrets)
	if len(result) != 0 {
		t.Errorf("expected empty result — explicit namespace should not fallback, got %v", result)
	}
}

// TestResolveValuesFrom_LooseFileFallback verifies that a resource without
// namespace IS found when valuesFrom doesn't specify a namespace (defaults
// to HR namespace). This covers legitimate loose-file resources without
// metadata.namespace that are part of the build output but not transformed
// by kustomize (e.g. read via os.ReadFile in buildSourcePath Case 1/3).
func TestResolveValuesFrom_LooseFileFallback(t *testing.T) {
	hr := HelmRelease{
		Metadata: ObjectMeta{Name: "app", Namespace: "podinfo"},
		Spec: HelmReleaseSpec{
			ValuesFrom: []interface{}{
				map[string]interface{}{
					"kind": "ConfigMap",
					"name": "app-values",
					// no namespace → defaults to HR namespace "podinfo"
				},
			},
		},
	}

	// ConfigMap without namespace (loose file, not kustomize-transformed).
	configMaps := []ConfigMap{
		{
			Metadata: ObjectMeta{Name: "app-values"}, // empty namespace
			Data:     map[string]string{"values.yaml": "key: value\n"},
		},
	}

	result := ResolveValuesFrom(hr, configMaps, nil)
	if result == nil || result["key"] == nil {
		t.Error("loose-file ConfigMap without namespace should match via fallback when entryNS defaults to HR namespace")
	}
	if result["key"] != "value" {
		t.Errorf("expected key=value, got %v", result["key"])
	}
}

// TestResolveValuesFrom_MixedConfigMapAndSecret verifies that ConfigMap values
// are resolved while Secret values are skipped, even when both are referenced
// in the same valuesFrom list.
func TestResolveValuesFrom_MixedConfigMapAndSecret(t *testing.T) {
	hr := HelmRelease{
		Metadata: ObjectMeta{Name: "app", Namespace: "flux-system"},
		Spec: HelmReleaseSpec{
			ValuesFrom: []interface{}{
				map[string]interface{}{
					"kind": "ConfigMap",
					"name": "app-config",
				},
				map[string]interface{}{
					"kind": "Secret",
					"name": "app-secrets",
				},
			},
		},
	}

	configMaps := []ConfigMap{
		{
			Metadata: ObjectMeta{Name: "app-config", Namespace: "flux-system"},
			Data: map[string]string{
				"values.yaml": "replicas: 2\n",
			},
		},
	}
	secrets := []Secret{
		{
			Metadata: ObjectMeta{Name: "app-secrets", Namespace: "flux-system"},
			StringData: map[string]string{
				"password": "leaked-if-not-fixed",
			},
		},
	}

	result := ResolveValuesFrom(hr, configMaps, secrets)

	// ConfigMap value should be present (parsed as YAML int from values.yaml key).
	if result["replicas"] != 2 {
		t.Errorf("ConfigMap value should be resolved, got %v", result["replicas"])
	}
	// Secret value must NOT be present.
	if v, ok := result["password"]; ok {
		t.Errorf("secret value leaked: %v", v)
	}
}
