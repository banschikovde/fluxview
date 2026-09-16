package git

import "testing"

func TestNormalizeGitURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"https", "https://github.com/kyverno/kyverno.git", "github.com/kyverno/kyverno"},
		{"https trailing slash", "https://github.com/kyverno/kyverno/", "github.com/kyverno/kyverno"},
		{"https no .git", "https://github.com/kyverno/kyverno", "github.com/kyverno/kyverno"},
		{"https mixed case host", "https://GitHub.Com/kyverno/kyverno.git", "github.com/kyverno/kyverno"},
		{"path case preserved", "https://github.com/Kyverno/Kyverno.git", "github.com/Kyverno/Kyverno"},
		{"ssh with user", "ssh://git@github.com/kyverno/kyverno.git", "github.com/kyverno/kyverno"},
		{"ssh with port", "ssh://git@gitlab.com:2222/group/repo.git", "gitlab.com:2222/group/repo"},
		{"scp-style", "git@github.com:kyverno/kyverno.git", "github.com/kyverno/kyverno"},
		{"scp-style no .git", "git@github.com:kyverno/kyverno", "github.com/kyverno/kyverno"},
		{"https with port", "https://gitlab.example.com:8443/group/repo.git", "gitlab.example.com:8443/group/repo"},
		{"https with token credentials", "https://user:token@github.com/org/repo.git", "github.com/org/repo"},
		{"file url", "file:///tmp/upstream/repo", "/tmp/upstream/repo"},
		{"file url with .git", "file:///tmp/upstream/repo.git", "/tmp/upstream/repo"},
		{"local path", "/tmp/upstream/repo", "/tmp/upstream/repo"},
		{"local path with colon after slash", "/tmp/a:b", "/tmp/a:b"},
		{"empty", "", ""},
		{"whitespace", "  https://github.com/org/repo.git  ", "github.com/org/repo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeGitURL(tt.raw); got != tt.want {
				t.Errorf("NormalizeGitURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestSameGitRepo(t *testing.T) {
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{"https vs scp", "https://github.com/kyverno/kyverno.git", "git@github.com:kyverno/kyverno", true},
		{"https vs ssh", "https://github.com/kyverno/kyverno", "ssh://git@github.com/kyverno/kyverno.git", true},
		{"different orgs", "https://github.com/kyverno/kyverno", "https://github.com/fluxcd/flux2", false},
		{"different path case", "https://github.com/kyverno/kyverno", "https://github.com/kyverno/Kyverno", false},
		{"different ports", "ssh://git@gitlab.com:2222/g/r.git", "ssh://git@gitlab.com/g/r.git", false},
		{"empty vs empty", "", "", false},
		{"empty vs set", "", "https://github.com/org/repo", false},
		{"local vs file url", "/tmp/upstream/repo", "file:///tmp/upstream/repo", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SameGitRepo(tt.a, tt.b); got != tt.want {
				t.Errorf("SameGitRepo(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
