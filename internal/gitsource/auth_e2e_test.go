package gitsource

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/gitsource/gitsourcetest"
)

// newHTTPSAuthedUpstream builds a two-commit upstream (tag v1.0.0 at the
// first commit, the branch tip ahead) served over authenticated git
// smart http (user fluxview, password s3cret); it returns the URL and
// the default branch name.
func newHTTPSAuthedUpstream(t *testing.T) (url, branch string) {
	t.Helper()
	up := newFixtureRepo(t)
	up.commit("config/crds/crd.yaml", "content: v1.0.0\n")
	up.tag("v1.0.0")
	up.commit("config/crds/crd.yaml", "content: moved\n")

	bare := gitsourcetest.BareClone(t, up.dir, "upstream")
	base := gitsourcetest.HTTPSBasicAuth(t, filepath.Dir(bare), "fluxview", "s3cret")
	return base + "/upstream.git", up.branch()
}

func ensureRef(t *testing.T, f *Fetcher, url string, ref *flux.GitRepositoryRef) error {
	t.Helper()
	_, err := f.Ensure(context.Background(), flux.GitRepository{
		Metadata: flux.ObjectMeta{Name: "upstream", Namespace: "test"},
		Spec:     flux.GitRepositorySpec{URL: url, Ref: ref},
	})
	return err
}

func TestE2E_HTTPSBasicAuthFetch(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	url, branch := newHTTPSAuthedUpstream(t)

	t.Run("env credentials fetch pinned and floating", func(t *testing.T) {
		t.Setenv(envGitUsername, "fluxview")
		t.Setenv(envGitPassword, "s3cret")
		t.Setenv(envGitCredentialHosts, hostname(parseGitEndpoint(url).host))
		f := NewFetcher(t.TempDir(), time.Hour)

		// Pinned tag: clone with basic auth, reused on the second call.
		ref := &flux.GitRepositoryRef{Tag: "v1.0.0"}
		first := mustEnsure(t, f, url, ref)
		if got := readCloneFile(t, first, "config/crds/crd.yaml"); got != "content: v1.0.0\n" {
			t.Errorf("tag clone holds %q", got)
		}
		if again := mustEnsure(t, f, url, ref); again != first {
			t.Errorf("pinned tag must be reused: %q vs %q", again, first)
		}

		// Floating branch: ls-remote resolution and shallow clone both go
		// through the authenticated transport.
		bdir := mustEnsure(t, f, url, &flux.GitRepositoryRef{Branch: branch})
		if got := readCloneFile(t, bdir, "config/crds/crd.yaml"); got != "content: moved\n" {
			t.Errorf("branch clone holds %q, want the moved tip", got)
		}
	})

	t.Run("no credentials fail with the 401 message", func(t *testing.T) {
		err := ensureRef(t, NewFetcher(t.TempDir(), time.Hour), url, &flux.GitRepositoryRef{Tag: "v1.0.0"})
		if err == nil || !strings.Contains(err.Error(), "private repository or bad credentials") {
			t.Fatalf("401 must surface the credentials hint, got: %v", err)
		}
	})

	t.Run("wrong password fails the same way", func(t *testing.T) {
		t.Setenv(envGitUsername, "fluxview")
		t.Setenv(envGitPassword, "wrong")
		t.Setenv(envGitCredentialHosts, hostname(parseGitEndpoint(url).host))
		err := ensureRef(t, NewFetcher(t.TempDir(), time.Hour), url, &flux.GitRepositoryRef{Tag: "v1.0.0"})
		if err == nil || !strings.Contains(err.Error(), "private repository or bad credentials") {
			t.Fatalf("rejected password must surface the credentials hint, got: %v", err)
		}
	})
}

func TestE2E_SSHKeyAuthFetch(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())

	up := newFixtureRepo(t)
	up.commit("config/crds/crd.yaml", "content: v1.0.0\n")
	up.tag("v1.0.0")
	up.commit("config/crds/crd.yaml", "content: moved\n")
	bare := gitsourcetest.BareClone(t, up.dir, "upstream")

	priv, signer := genEd25519(t)
	base, hostSigner := gitsourcetest.SSHGitServer(t, signer.PublicKey())
	url := base + bare
	hostWithPort := strings.TrimPrefix(base, "ssh://git@")

	// Strict known_hosts with the server's real host key + the client key
	// from env — the full production-shaped setup.
	t.Setenv(EnvSSHKnownHosts, writeKnownHosts(t,
		filepath.Join(t.TempDir(), "known_hosts"), hostWithPort, hostSigner.PublicKey()))
	t.Setenv(envSSHKey, writeKeyFile(t, filepath.Join(t.TempDir(), "id_e2e"), priv, ""))

	t.Run("clone, pinned reuse and floating resolution", func(t *testing.T) {
		f := NewFetcher(t.TempDir(), time.Hour)

		ref := &flux.GitRepositoryRef{Tag: "v1.0.0"}
		first := mustEnsure(t, f, url, ref)
		if got := readCloneFile(t, first, "config/crds/crd.yaml"); got != "content: v1.0.0\n" {
			t.Errorf("tag clone holds %q", got)
		}
		if again := mustEnsure(t, f, url, ref); again != first {
			t.Errorf("pinned tag must be reused: %q vs %q", again, first)
		}

		// Floating branch: authenticated ls-remote + shallow clone.
		bdir := mustEnsure(t, f, url, &flux.GitRepositoryRef{Branch: up.branch()})
		if got := readCloneFile(t, bdir, "config/crds/crd.yaml"); got != "content: moved\n" {
			t.Errorf("branch clone holds %q, want the moved tip", got)
		}
	})

	t.Run("rejected key names the credential source", func(t *testing.T) {
		// A second server authorizing a DIFFERENT key: the handshake
		// rejects ours.
		_, other := genEd25519(t)
		base2, hostSigner2 := gitsourcetest.SSHGitServer(t, other.PublicKey())
		t.Setenv(EnvSSHKnownHosts, writeKnownHosts(t,
			filepath.Join(t.TempDir(), "kh2"), strings.TrimPrefix(base2, "ssh://git@"), hostSigner2.PublicKey()))

		err := ensureRef(t, NewFetcher(t.TempDir(), time.Hour), base2+bare, &flux.GitRepositoryRef{Tag: "v1.0.0"})
		if err == nil || !strings.Contains(err.Error(), "SSH authentication failed") {
			t.Fatalf("a rejected key must be named as such, got: %v", err)
		}
		if err == nil || !strings.Contains(err.Error(), envSSHKey) {
			t.Fatalf("the message must name the credential source, got: %v", err)
		}
	})

	t.Run("changed host key fails verification", func(t *testing.T) {
		// known_hosts holding some other key for the same host: the MITM
		// case must fail loudly.
		_, decoy := genEd25519(t)
		t.Setenv(EnvSSHKnownHosts, writeKnownHosts(t,
			filepath.Join(t.TempDir(), "kh_decoy"), hostWithPort, decoy.PublicKey()))

		err := ensureRef(t, NewFetcher(t.TempDir(), time.Hour), url, &flux.GitRepositoryRef{Tag: "v1.0.0"})
		if err == nil || !strings.Contains(err.Error(), "host key verification failed") {
			t.Fatalf("a changed host key must fail verification, got: %v", err)
		}
	})
}

// TestE2E_CacheStaysCredentialFree pins the security property of ТЗ-1
// §2.6: nothing secret ever lands in the cache — the clone directory is
// keyed by the normalized URL only, and the seed names the manifest URL.
func TestE2E_CacheStaysCredentialFree(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	url, _ := newHTTPSAuthedUpstream(t)
	t.Setenv(envGitUsername, "fluxview")
	t.Setenv(envGitPassword, "s3cret")
	t.Setenv(envGitCredentialHosts, hostname(parseGitEndpoint(url).host))

	cache := t.TempDir()
	f := NewFetcher(cache, time.Hour)
	dir := mustEnsure(t, f, url, &flux.GitRepositoryRef{Tag: "v1.0.0"})

	seed, err := os.ReadFile(filepath.Join(dir, seedFileName))
	if err != nil {
		t.Fatalf("reading seed: %v", err)
	}
	for _, secret := range []string{"s3cret", "fluxview:s3cret", "Authorization"} {
		if strings.Contains(string(seed), secret) || strings.Contains(dir, secret) {
			t.Errorf("secret %q leaked into the cache (dir %s, seed %s)", secret, dir, seed)
		}
	}
	if !strings.Contains(string(seed), "/upstream.git") {
		t.Errorf("seed must name the manifest URL, got: %s", seed)
	}
}
