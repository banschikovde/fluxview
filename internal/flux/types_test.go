package flux

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestGitRepositoryRefRefString(t *testing.T) {
	ref := func(commit, tag, branch, semver string) *GitRepositoryRef {
		return &GitRepositoryRef{Commit: commit, Tag: tag, Branch: branch, Semver: semver}
	}

	tests := []struct {
		name string
		ref  *GitRepositoryRef
		want string
	}{
		{"nil ref is HEAD", nil, ""},
		{"empty ref is HEAD", &GitRepositoryRef{}, ""},
		{"commit", ref("abc123", "", "", ""), "commit:abc123"},
		{"commit wins over tag and branch", ref("abc123", "v1.0.0", "main", ""), "commit:abc123"},
		{"tag", ref("", "v1.19.1", "", ""), "tag:v1.19.1"},
		{"tag wins over branch and semver", ref("", "v1.0.0", "main", ">=1.0"), "tag:v1.0.0"},
		{"branch", ref("", "", "main", ""), "branch:main"},
		{"branch wins over semver", ref("", "", "release", ">=1.0"), "branch:release"},
		{"semver", ref("", "", "", ">=1.19.0"), "semver:>=1.19.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.ref.RefString(); got != tt.want {
				t.Errorf("RefString() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGitRepositoryRefIsPinned(t *testing.T) {
	tests := []struct {
		name string
		ref  *GitRepositoryRef
		want bool
	}{
		{"nil ref (HEAD) is floating", nil, false},
		{"empty ref is floating", &GitRepositoryRef{}, false},
		{"commit is pinned", &GitRepositoryRef{Commit: "abc123"}, true},
		{"tag is pinned", &GitRepositoryRef{Tag: "v1.19.1"}, true},
		{"branch is floating", &GitRepositoryRef{Branch: "main"}, false},
		{"semver is floating", &GitRepositoryRef{Semver: ">=1.0"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.ref.IsPinned(); got != tt.want {
				t.Errorf("IsPinned() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetSecretValue(t *testing.T) {
	tests := []struct {
		name     string
		secret   Secret
		key      string
		expected string
	}{
		{
			name: "value from stringData",
			secret: Secret{
				Metadata: ObjectMeta{Name: "test", Namespace: "default"},
				StringData: map[string]string{
					"password": "plain-text-password",
				},
			},
			key:      "password",
			expected: "plain-text-password",
		},
		{
			name: "value from base64-encoded data",
			secret: Secret{
				Metadata: ObjectMeta{Name: "test", Namespace: "default"},
				Data: map[string]string{
					"password": "cGxhaW4tdGV4dC1wYXNzd29yZA==", // base64 of "plain-text-password"
				},
			},
			key:      "password",
			expected: "plain-text-password",
		},
		{
			name: "stringData takes precedence over data",
			secret: Secret{
				Metadata: ObjectMeta{Name: "test", Namespace: "default"},
				StringData: map[string]string{
					"password": "from-string-data",
				},
				Data: map[string]string{
					"password": "ZnJvbS1kYXRh", // base64 of "from-data"
				},
			},
			key:      "password",
			expected: "from-string-data",
		},
		{
			name: "non-existent key",
			secret: Secret{
				Metadata: ObjectMeta{Name: "test", Namespace: "default"},
				Data: map[string]string{
					"other": "value",
				},
			},
			key:      "nonexistent",
			expected: "",
		},
		{
			name: "invalid base64 in data",
			secret: Secret{
				Metadata: ObjectMeta{Name: "test", Namespace: "default"},
				Data: map[string]string{
					"password": "not-valid-base64!!!",
				},
			},
			key:      "password",
			expected: "",
		},
		{
			name: "empty secret",
			secret: Secret{
				Metadata: ObjectMeta{Name: "test", Namespace: "default"},
			},
			key:      "any",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.secret.GetSecretValue(tt.key)
			if result != tt.expected {
				t.Errorf("GetSecretValue(%q) = %q, want %q", tt.key, result, tt.expected)
			}
		})
	}
}

func TestOCIRepositoryRefRefString(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  *OCIRepositoryRef
		want string
	}{
		{name: "nil", ref: nil, want: ""},
		{name: "empty", ref: &OCIRepositoryRef{}, want: ""},
		{name: "digest", ref: &OCIRepositoryRef{Digest: "sha256:abc", Tag: "v1"}, want: "digest:sha256:abc"},
		{name: "semver", ref: &OCIRepositoryRef{Semver: ">=1.0.0", Tag: "v1"}, want: "semver:>=1.0.0"},
		{name: "tag", ref: &OCIRepositoryRef{Tag: "v1"}, want: "tag:v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ref.RefString(); got != tc.want {
				t.Errorf("RefString() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOCIRepositoryRefIsPinned(t *testing.T) {
	if (&OCIRepositoryRef{Tag: "v1"}).IsPinned() {
		t.Error("a tag is mutable — not pinned")
	}
	if !(&OCIRepositoryRef{Digest: "sha256:abc"}).IsPinned() {
		t.Error("a digest is pinned")
	}
	var nilRef *OCIRepositoryRef
	if nilRef.IsPinned() {
		t.Error("a nil ref is not pinned")
	}
}

func TestOCIRepositorySpecParsesFetchFields(t *testing.T) {
	manifest := `apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: envoy
  namespace: flux-system
spec:
  url: oci://docker.io/envoyproxy/gateway-helm
  insecure: true
  provider: generic
  secretRef:
    name: regcred
  layerSelector:
    mediaType: application/vnd.cncf.helm.chart.content.v1.tar+gzip
    operation: copy
  ref:
    tag: v1.10.0
`
	// Decoded through the same YAML unmarshalling the parser uses.
	var repo OCIRepository
	if err := yaml.Unmarshal([]byte(manifest), &repo); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !repo.Spec.Insecure || repo.Spec.Provider != "generic" {
		t.Errorf("insecure/provider not parsed: %+v", repo.Spec)
	}
	if repo.Spec.SecretRef == nil || repo.Spec.SecretRef.Name != "regcred" {
		t.Errorf("secretRef not parsed: %+v", repo.Spec.SecretRef)
	}
	if repo.Spec.LayerSelector == nil ||
		repo.Spec.LayerSelector.MediaType != "application/vnd.cncf.helm.chart.content.v1.tar+gzip" ||
		repo.Spec.LayerSelector.Operation != "copy" {
		t.Errorf("layerSelector not parsed: %+v", repo.Spec.LayerSelector)
	}
	if repo.Spec.Ref == nil || repo.Spec.Ref.Tag != "v1.10.0" || repo.Spec.Ref.RefString() != "tag:v1.10.0" {
		t.Errorf("ref.tag not parsed: %+v", repo.Spec.Ref)
	}
}
