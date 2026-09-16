package flux

import (
	"testing"
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
