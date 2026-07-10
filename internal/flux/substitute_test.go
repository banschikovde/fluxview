package flux

import (
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
				"key1": "value1",
				"key2": "value2",
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

// TestResolveValuesFrom_SecretsNotResolved verifies that secret values are
// NOT injected into Helm rendering. This prevents leakage of real secret
// values through rendered non-Secret resources (ConfigMaps, annotations, etc.)
// that RedactSecrets does not cover.
func TestResolveValuesFrom_SecretsNotResolved(t *testing.T) {
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
				"password": "c2VjcmV0LXBhc3N3b3Jk", // base64 of "secret-password"
			},
			StringData: map[string]string{
				"token": "real-token-value",
			},
		},
	}

	result := ResolveValuesFrom(hr, nil, secrets)

	// Secret values must NOT appear in the result map.
	if result != nil {
		if v, ok := result["password"]; ok {
			t.Errorf("secret value for 'password' leaked into Helm values: %v", v)
		}
		if v, ok := result["token"]; ok {
			t.Errorf("secret value for 'token' leaked into Helm values: %v", v)
		}
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
				"replicas": "2",
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

	// ConfigMap value should be present.
	if result["replicas"] != "2" {
		t.Errorf("ConfigMap value should be resolved, got %v", result["replicas"])
	}
	// Secret value must NOT be present.
	if v, ok := result["password"]; ok {
		t.Errorf("secret value leaked: %v", v)
	}
}
