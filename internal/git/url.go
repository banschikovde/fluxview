package git

import "strings"

// NormalizeGitURL canonicalizes a git remote URL for repository identity
// comparisons: scheme, credentials and a trailing ".git" are dropped, the
// host (with port) is lowercased, trailing slashes trimmed. The scp-style
// form, the ssh/https schemes and mixed spellings of one repository all
// normalize to the same "host[:port]/path" string:
//
//	git@github.com:kyverno/kyverno.git   → github.com/kyverno/kyverno
//	ssh://git@github.com/kyverno/kyverno → github.com/kyverno/kyverno
//	https://GitHub.com/kyverno/kyverno/  → github.com/kyverno/kyverno
//
// file:// URLs and plain local paths normalize to the absolute-looking path
// itself, so a local fixture and its file:// spelling stay comparable.
func NormalizeGitURL(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimRight(s, "/")
	if s == "" {
		return ""
	}

	// file:// URL → local path.
	if rest, ok := strings.CutPrefix(s, "file://"); ok {
		return strings.TrimSuffix(rest, ".git")
	}

	// scheme://[userinfo@]host[:port]/path
	if i := strings.Index(s, "://"); i >= 0 {
		rest := s[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			rest = rest[at+1:]
		}
		host, path := rest, ""
		if slash := strings.Index(rest, "/"); slash >= 0 {
			host, path = rest[:slash], rest[slash+1:]
		}
		if host == "" {
			return strings.TrimSuffix(path, ".git")
		}
		return strings.ToLower(host) + "/" + strings.TrimSuffix(path, ".git")
	}

	// scp-style [user@]host:path — the colon must precede any slash to
	// avoid mangling local paths or drive letters.
	if at := strings.LastIndex(s, "@"); at >= 0 {
		s = s[at+1:]
	}
	if colon := strings.Index(s, ":"); colon >= 0 && (colon < strings.Index(s, "/") || !strings.Contains(s, "/")) {
		host, path := s[:colon], s[colon+1:]
		if host != "" {
			return strings.ToLower(host) + "/" + strings.TrimSuffix(path, ".git")
		}
	}

	// Local path or an already-canonical host/path spelling.
	return strings.TrimSuffix(s, ".git")
}

// SameGitRepo reports whether two git URLs point at the same repository,
// comparing their normalized forms. Two empty URLs identify nothing and
// are never the same repository.
func SameGitRepo(a, b string) bool {
	na, nb := NormalizeGitURL(a), NormalizeGitURL(b)
	return na != "" && na == nb
}
