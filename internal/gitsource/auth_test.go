package gitsource

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// clearAuthEnv neutralizes every auth-relevant environment variable so
// tests never see the developer machine's agent, keys or netrc.
func clearAuthEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{
		envSSHKey, envSSHPassphrase, EnvSSHKnownHosts, EnvSSHAcceptNew,
		envGitUsername, envGitPassword, envGitToken,
		envGitCredentialHosts, "SSH_AUTH_SOCK",
	} {
		t.Setenv(v, "")
	}
}

// genEd25519 generates an ssh signer for tests.
func genEd25519(t *testing.T) (ed25519.PrivateKey, ssh.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("NewSignerFromKey: %v", err)
	}
	return priv, signer
}

// writeKeyFile writes a private key in OpenSSH PEM form, encrypted when a
// passphrase is given, and returns the path.
func writeKeyFile(t *testing.T, path string, priv ed25519.PrivateKey, passphrase string) string {
	t.Helper()
	var block *pem.Block
	var err error
	if passphrase != "" {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	} else {
		block, err = ssh.MarshalPrivateKey(priv, "")
	}
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("writing key %s: %v", path, err)
	}
	return path
}

// writeKnownHosts writes a known_hosts file trusting hostWithPort's key.
func writeKnownHosts(t *testing.T, path, hostWithPort string, key ssh.PublicKey) string {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating known_hosts %s: %v", path, err)
	}
	defer f.Close()
	if err := knownhosts.WriteKnownHost(f, hostWithPort, &net.TCPAddr{}, key); err != nil {
		t.Fatalf("WriteKnownHost: %v", err)
	}
	return path
}

// emptyKnownHosts creates an existing but empty known_hosts file.
func emptyKnownHosts(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("creating empty known_hosts: %v", err)
	}
	t.Setenv(EnvSSHKnownHosts, path)
	return path
}

// startTestAgent serves a keyring ssh-agent over a unix socket and
// returns the socket path plus a counter of live agent connections. The
// agent starts with no keys; withKey adds one.
func startTestAgent(t *testing.T, withKey bool) (string, *atomic.Int32) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "agent.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listening for test agent: %v", err)
	}
	keyring := agent.NewKeyring()
	if withKey {
		priv, _ := genEd25519(t)
		if err := keyring.Add(agent.AddedKey{PrivateKey: priv}); err != nil {
			t.Fatalf("adding key to test agent: %v", err)
		}
	}
	var live atomic.Int32
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			live.Add(1)
			go func() {
				defer live.Add(-1)
				agent.ServeAgent(keyring, c)
			}()
		}
	}()
	t.Cleanup(func() { l.Close() })
	return sock, &live
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestParseGitEndpoint(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want gitEndpoint
	}{
		{"file url", "file:///srv/repos/crds", gitEndpoint{local: true, scheme: "file"}},
		{"local path", "../repos/crds", gitEndpoint{local: true}},
		{"absolute path", "/srv/repos/crds", gitEndpoint{local: true}},
		{"ssh url", "ssh://git@git.example.com:2222/platform/crds.git", gitEndpoint{scheme: "ssh", user: "git", host: "git.example.com:2222"}},
		{"ssh url no user", "ssh://GitLab.example.com/platform/crds", gitEndpoint{scheme: "ssh", host: "gitlab.example.com"}},
		{"scp form", "git@git.example.com:platform/crds.git", gitEndpoint{scp: true, user: "git", host: "git.example.com"}},
		{"scp no user", "git.example.com:platform/crds.git", gitEndpoint{scp: true, host: "git.example.com"}},
		{"scp with slash in path", "git.example.com:platform/sub/dir", gitEndpoint{scp: true, host: "git.example.com"}},
		{"path with colon after slash", "relative/dir:name", gitEndpoint{local: true}},
		{"https", "https://GitLab.example.com/platform/crds.git", gitEndpoint{scheme: "https", host: "gitlab.example.com"}},
		{"https creds in url dropped", "https://user:pw@git.example.com/crds", gitEndpoint{scheme: "https", host: "git.example.com"}},
		{"git scheme", "git://git.example.com/crds", gitEndpoint{scheme: "git", host: "git.example.com"}},
		{"empty", "  ", gitEndpoint{local: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseGitEndpoint(tt.url); got != tt.want {
				t.Errorf("parseGitEndpoint(%q) = %+v, want %+v", tt.url, got, tt.want)
			}
		})
	}
}

func TestHostWithPort(t *testing.T) {
	tests := []struct{ host, want string }{
		{"git.example.com", "git.example.com:22"},
		{"git.example.com:2222", "git.example.com:2222"},
		{"[::1]", "[::1]:22"},
		{"[::1]:2222", "[::1]:2222"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := (gitEndpoint{host: tt.host}).hostWithPort(); got != tt.want {
			t.Errorf("hostWithPort(%q) = %q, want %q", tt.host, got, tt.want)
		}
	}
	if got := hostname("git.example.com:8443"); got != "git.example.com" {
		t.Errorf("hostname(git.example.com:8443) = %q", got)
	}
	if got := hostname("[::1]:8443"); got != "[::1]" {
		t.Errorf("hostname([::1]:8443) = %q", got)
	}
	if got := hostname("git.example.com"); got != "git.example.com" {
		t.Errorf("hostname(git.example.com) = %q", got)
	}
}

func TestResolveAuth_NoAuthSchemes(t *testing.T) {
	clearAuthEnv(t)
	r := newAuthResolver()
	for _, url := range []string{
		"file:///srv/repos/crds",
		"../fixtures/crds",
		"git://git.example.com/crds",
	} {
		auth, err := r.resolveAuth(url)
		if auth != nil || err != nil {
			t.Errorf("resolveAuth(%q) = %v, %v; want nil, nil", url, auth, err)
		}
	}
}

func TestResolveAuth_SSHKeyFromEnv(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	emptyKnownHosts(t)
	keyPath := writeKeyFile(t, filepath.Join(t.TempDir(), "id_custom"), mustPriv(t), "")
	t.Setenv(envSSHKey, keyPath)

	r := newAuthResolver()
	auth, err := r.resolveAuth("git@git.example.com:platform/crds.git")
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	pk, ok := auth.(*gitssh.PublicKeys)
	if !ok {
		t.Fatalf("want *ssh.PublicKeys, got %T", auth)
	}
	if pk.User != "git" {
		t.Errorf("default user = %q, want git", pk.User)
	}
	if pk.HostKeyCallback == nil {
		t.Error("host key callback must be wired")
	}

	// An explicit ssh:// user wins over the default (fresh resolver: the
	// host above is already memoized with its own user).
	r2 := newAuthResolver()
	auth, err = r2.resolveAuth("ssh://deploy@git.example.com/platform/crds.git")
	if err != nil {
		t.Fatalf("resolveAuth with explicit user: %v", err)
	}
	if pk := auth.(*gitssh.PublicKeys); pk.User != "deploy" {
		t.Errorf("explicit user = %q, want deploy", pk.User)
	}
}

func TestResolveAuth_SSHKeyFromEnvMissingFile(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	emptyKnownHosts(t)
	missing := filepath.Join(t.TempDir(), "not-there")
	t.Setenv(envSSHKey, missing)

	_, err := newAuthResolver().resolveAuth("git@git.example.com:crds.git")
	if err == nil || !strings.Contains(err.Error(), missing) || !strings.Contains(err.Error(), envSSHKey) {
		t.Errorf("error must name the missing key file and env var, got: %v", err)
	}
}

func TestResolveAuth_SSHEncryptedKey(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	emptyKnownHosts(t)

	priv, _ := genEd25519(t)
	keyPath := writeKeyFile(t, filepath.Join(t.TempDir(), "id_enc"), priv, "open sesame")
	t.Setenv(envSSHKey, keyPath)

	t.Run("passphrase resolves", func(t *testing.T) {
		t.Setenv(envSSHPassphrase, "open sesame")
		auth, err := newAuthResolver().resolveAuth("git@git.example.com:crds.git")
		if err != nil {
			t.Fatalf("resolveAuth with passphrase: %v", err)
		}
		if _, ok := auth.(*gitssh.PublicKeys); !ok {
			t.Fatalf("want *ssh.PublicKeys, got %T", auth)
		}
	})

	t.Run("missing passphrase hints the env", func(t *testing.T) {
		_, err := newAuthResolver().resolveAuth("git@git.example.com:crds.git")
		if err == nil || !strings.Contains(err.Error(), envSSHPassphrase) {
			t.Errorf("error must hint FLUXVIEW_GIT_SSH_PASSPHRASE, got: %v", err)
		}
		if err != nil && strings.Contains(err.Error(), "open sesame") {
			t.Errorf("error must not leak the passphrase: %v", err)
		}
	})
}

func TestResolveAuth_SSHAgent(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	emptyKnownHosts(t)
	sock, live := startTestAgent(t, true)
	t.Setenv("SSH_AUTH_SOCK", sock)

	r := newAuthResolver()
	auth, err := r.resolveAuth("git@git.example.com:platform/crds.git")
	if err != nil {
		t.Fatalf("resolveAuth via agent: %v", err)
	}
	cb, ok := auth.(*gitssh.PublicKeysCallback)
	if !ok {
		t.Fatalf("want *ssh.PublicKeysCallback, got %T", auth)
	}
	if cb.User != "git" || cb.HostKeyCallback == nil {
		t.Errorf("agent auth user=%q, hostKeyCallback=%v", cb.User, cb.HostKeyCallback)
	}
	waitFor(t, "agent connection", func() bool { return live.Load() == 1 })

	// A second resolve of the same host neither re-dials the agent nor
	// resolves again (memoized per host).
	if _, err := r.resolveAuth("ssh://git@git.example.com/other.git"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if live.Load() != 1 {
		t.Errorf("memoized resolve re-dialed the agent, live=%d", live.Load())
	}

	// Close tears the agent connection down and drops memoized results.
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, "agent connection closed", func() bool { return live.Load() == 0 })

	if _, err := r.resolveAuth("git@git.example.com:crds.git"); err != nil {
		t.Fatalf("resolve after Close re-dials: %v", err)
	}
	waitFor(t, "agent re-dial", func() bool { return live.Load() == 1 })
	if err := r.Close(); err != nil {
		t.Errorf("Close must be safe twice: %v", err)
	}
}

func TestResolveAuth_SSHIdentityFiles(t *testing.T) {
	clearAuthEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	emptyKnownHosts(t)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o755); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}

	edPriv, edSigner := genEd25519(t)
	writeKeyFile(t, filepath.Join(sshDir, "id_ed25519"), edPriv, "")

	t.Run("first existing identity resolves", func(t *testing.T) {
		auth, err := newAuthResolver().resolveAuth("git@git.example.com:crds.git")
		if err != nil {
			t.Fatalf("resolveAuth: %v", err)
		}
		pk, ok := auth.(*gitssh.PublicKeys)
		if !ok {
			t.Fatalf("want *ssh.PublicKeys, got %T", auth)
		}
		if got, want := ssh.FingerprintSHA256(pk.Signer.PublicKey()), ssh.FingerprintSHA256(edSigner.PublicKey()); got != want {
			t.Errorf("resolved key fingerprint %s, want the id_ed25519 one %s", got, want)
		}
	})

	t.Run("falls back to id_rsa", func(t *testing.T) {
		if err := os.Remove(filepath.Join(sshDir, "id_ed25519")); err != nil {
			t.Fatalf("removing id_ed25519: %v", err)
		}
		rsaPriv, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("rsa.GenerateKey: %v", err)
		}
		rsaSigner, err := ssh.NewSignerFromKey(rsaPriv)
		if err != nil {
			t.Fatalf("NewSignerFromKey(rsa): %v", err)
		}
		writeKeyFile(t, filepath.Join(sshDir, "id_rsa"), mustPriv(t), "") // wrong key shape is fine, it must not be picked
		_ = rsaSigner
		// Replace id_rsa with the real RSA key so the fallback is unambiguous.
		os.Remove(filepath.Join(sshDir, "id_rsa"))
		block, err := ssh.MarshalPrivateKey(rsaPriv, "")
		if err != nil {
			t.Fatalf("MarshalPrivateKey(rsa): %v", err)
		}
		if err := os.WriteFile(filepath.Join(sshDir, "id_rsa"), pem.EncodeToMemory(block), 0o600); err != nil {
			t.Fatalf("writing id_rsa: %v", err)
		}

		auth, err := newAuthResolver().resolveAuth("git@git.example.com:crds.git")
		if err != nil {
			t.Fatalf("resolveAuth: %v", err)
		}
		pk := auth.(*gitssh.PublicKeys)
		if got, want := ssh.FingerprintSHA256(pk.Signer.PublicKey()), ssh.FingerprintSHA256(rsaSigner.PublicKey()); got != want {
			t.Errorf("fallback key fingerprint %s, want the id_rsa one %s", got, want)
		}
	})

	t.Run("broken identity errors naming the file", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(sshDir, "id_rsa"), []byte("not a key"), 0o600); err != nil {
			t.Fatalf("breaking id_rsa: %v", err)
		}
		_, err := newAuthResolver().resolveAuth("git@git.example.com:crds.git")
		if err == nil || !strings.Contains(err.Error(), "id_rsa") || !strings.Contains(err.Error(), envSSHPassphrase) {
			t.Errorf("error must name the identity file and hint the passphrase env, got: %v", err)
		}
	})
}

func TestResolveAuth_SSHNoCredentials(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	emptyKnownHosts(t)

	_, err := newAuthResolver().resolveAuth("git@git.example.com:crds.git")
	if err == nil {
		t.Fatal("resolveAuth without any credentials must fail")
	}
	for _, want := range []string{
		"no SSH credentials found for git@git.example.com:22",
		envSSHKey, "SSH_AUTH_SOCK", "id_ed25519",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %q", err, want)
		}
	}
}

func TestResolveAuth_SSHKeylessAgentFallsThroughToIdentities(t *testing.T) {
	clearAuthEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	emptyKnownHosts(t)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o755); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}
	edPriv, edSigner := genEd25519(t)
	writeKeyFile(t, filepath.Join(sshDir, "id_ed25519"), edPriv, "")

	sock, live := startTestAgent(t, false) // reachable agent, zero keys
	t.Setenv("SSH_AUTH_SOCK", sock)

	auth, err := newAuthResolver().resolveAuth("git@git.example.com:crds.git")
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	pk, ok := auth.(*gitssh.PublicKeys)
	if !ok {
		t.Fatalf("a keyless agent must fall through to identity files, got %T", auth)
	}
	if got, want := ssh.FingerprintSHA256(pk.Signer.PublicKey()), ssh.FingerprintSHA256(edSigner.PublicKey()); got != want {
		t.Errorf("fell through to the wrong identity: %s, want %s", got, want)
	}
	waitFor(t, "keyless agent connection closed", func() bool { return live.Load() == 0 })
}

func TestResolve_RecordsCredentialSource(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	emptyKnownHosts(t)

	t.Run("env key", func(t *testing.T) {
		t.Setenv(envSSHKey, writeKeyFile(t, filepath.Join(t.TempDir(), "id_custom"), mustPriv(t), ""))
		out, err := newAuthResolver().resolve("git@git.example.com:crds.git")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if !out.ssh || out.keySource != envSSHKey {
			t.Errorf("outcome ssh=%v keySource=%q, want ssh/FLUXVIEW_GIT_SSH_KEY", out.ssh, out.keySource)
		}
	})

	t.Run("agent", func(t *testing.T) {
		sock, _ := startTestAgent(t, true)
		t.Setenv("SSH_AUTH_SOCK", sock)
		out, err := newAuthResolver().resolve("git@git.example.com:crds.git")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if !out.ssh || out.keySource != "ssh-agent" {
			t.Errorf("outcome ssh=%v keySource=%q, want ssh/ssh-agent", out.ssh, out.keySource)
		}
	})

	t.Run("identity file", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		sshDir := filepath.Join(home, ".ssh")
		if err := os.MkdirAll(sshDir, 0o755); err != nil {
			t.Fatalf("mkdir .ssh: %v", err)
		}
		writeKeyFile(t, filepath.Join(sshDir, "id_ed25519"), mustPriv(t), "")
		out, err := newAuthResolver().resolve("git@git.example.com:crds.git")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if !out.ssh || out.keySource != "~/.ssh/id_ed25519" {
			t.Errorf("outcome ssh=%v keySource=%q, want ssh/~/.ssh/id_ed25519", out.ssh, out.keySource)
		}
	})

	t.Run("http outcome is not ssh", func(t *testing.T) {
		t.Setenv(envGitCredentialHosts, "git.example.com")
		t.Setenv(envGitUsername, "alice")
		t.Setenv(envGitPassword, "pw")
		out, err := newAuthResolver().resolve("https://git.example.com/crds.git")
		if err != nil || out.ssh || out.auth == nil {
			t.Errorf("http outcome = %+v, %v; want a non-ssh one with auth", out, err)
		}
	})
}

func TestResolveAuth_MemoizedPerHost(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	emptyKnownHosts(t)
	keyPath := writeKeyFile(t, filepath.Join(t.TempDir(), "id_custom"), mustPriv(t), "")
	t.Setenv(envSSHKey, keyPath)

	r := newAuthResolver()
	first, err := r.resolveAuth("git@git.example.com:crds.git")
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}

	// The environment changes and the key file disappears — the host is
	// already resolved, so nothing re-reads them.
	os.Remove(keyPath)
	t.Setenv(envSSHKey, "")
	second, err := r.resolveAuth("ssh://git@git.example.com/other.git")
	if err != nil {
		t.Fatalf("memoized resolve: %v", err)
	}
	if first != second {
		t.Error("same host must resolve to the memoized auth method")
	}

	// A different host resolves fresh — and now fails on the removed key.
	if _, err := r.resolveAuth("git@other.example.com:crds.git"); err == nil {
		t.Error("a different host must resolve fresh and see the broken environment")
	}
}

// Resolutions of the same host over different transports (https basic
// auth vs ssh keys) must never share a memoized outcome: the auth objects
// are not interchangeable, and an ssh failure must not poison https.
func TestResolveAuth_MemoizationSeparatesTransports(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	emptyKnownHosts(t)
	keyPath := writeKeyFile(t, filepath.Join(t.TempDir(), "id_custom"), mustPriv(t), "")
	t.Setenv(envGitUsername, "alice")
	t.Setenv(envGitPassword, "hunter2")
	t.Setenv(envSSHKey, keyPath)
	t.Setenv(envGitCredentialHosts, "git.example.com")

	t.Run("https first must not leak basic auth into ssh", func(t *testing.T) {
		r := newAuthResolver()
		if _, err := r.resolveAuth("https://git.example.com/crds.git"); err != nil {
			t.Fatalf("https resolve: %v", err)
		}
		auth, err := r.resolveAuth("git@git.example.com:crds.git")
		if err != nil {
			t.Fatalf("ssh resolve: %v", err)
		}
		if _, ok := auth.(*githttp.BasicAuth); ok {
			t.Fatal("ssh resolve returned the memoized https basic auth")
		}
		if _, ok := auth.(*gitssh.PublicKeys); !ok {
			t.Fatalf("ssh resolve: want *ssh.PublicKeys, got %T", auth)
		}
	})

	t.Run("ssh first must not leak keys into https", func(t *testing.T) {
		r := newAuthResolver()
		if _, err := r.resolveAuth("ssh://git@git.example.com/crds.git"); err != nil {
			t.Fatalf("ssh resolve: %v", err)
		}
		auth, err := r.resolveAuth("https://git.example.com/crds.git")
		if err != nil {
			t.Fatalf("https resolve: %v", err)
		}
		ba, ok := auth.(*githttp.BasicAuth)
		if !ok {
			t.Fatalf("https resolve: want *http.BasicAuth, got %T", auth)
		}
		if ba.Username != "alice" || ba.Password != "hunter2" {
			t.Errorf("basic auth = %q/%q", ba.Username, ba.Password)
		}
	})

	t.Run("ssh dead end must not fail https of the same host", func(t *testing.T) {
		t.Setenv(envSSHKey, "")
		r := newAuthResolver()
		if _, err := r.resolveAuth("git@git.example.com:crds.git"); err == nil {
			t.Fatal("ssh resolve without credentials must fail")
		}
		auth, err := r.resolveAuth("https://git.example.com/crds.git")
		if err != nil {
			t.Fatalf("https resolve after ssh failure: %v", err)
		}
		if auth == nil {
			t.Fatal("https resolve returned no auth despite env credentials")
		}
	})
}

func TestResolveAuth_HTTP(t *testing.T) {
	clearAuthEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Run("username and password", func(t *testing.T) {
		t.Setenv(envGitCredentialHosts, "git.example.com")
		t.Setenv(envGitUsername, "alice")
		t.Setenv(envGitPassword, "hunter2")
		auth, err := newAuthResolver().resolveAuth("https://git.example.com/crds.git")
		if err != nil {
			t.Fatalf("resolveAuth: %v", err)
		}
		ba, ok := auth.(*githttp.BasicAuth)
		if !ok {
			t.Fatalf("want *http.BasicAuth, got %T", auth)
		}
		if ba.Username != "alice" || ba.Password != "hunter2" {
			t.Errorf("basic auth = %q/%q", ba.Username, ba.Password)
		}
	})

	t.Run("env pair wins over token", func(t *testing.T) {
		t.Setenv(envGitCredentialHosts, "git.example.com")
		t.Setenv(envGitUsername, "alice")
		t.Setenv(envGitPassword, "hunter2")
		t.Setenv(envGitToken, "glpat-tok")
		auth, _ := newAuthResolver().resolveAuth("https://git.example.com/crds.git")
		if ba := auth.(*githttp.BasicAuth); ba.Username != "alice" || ba.Password != "hunter2" {
			t.Errorf("env pair must win, got %q/%q", ba.Username, ba.Password)
		}
	})

	t.Run("token becomes oauth2", func(t *testing.T) {
		t.Setenv(envGitCredentialHosts, "git.example.com")
		t.Setenv(envGitToken, "glpat-tok")
		auth, _ := newAuthResolver().resolveAuth("https://git.example.com/crds.git")
		ba, ok := auth.(*githttp.BasicAuth)
		if !ok {
			t.Fatalf("want *http.BasicAuth, got %T", auth)
		}
		if ba.Username != "oauth2" || ba.Password != "glpat-tok" {
			t.Errorf("token auth = %q/%q, want oauth2/glpat-tok", ba.Username, ba.Password)
		}
	})

	t.Run("credentials withheld without an allowlist", func(t *testing.T) {
		t.Setenv(envGitToken, "glpat-tok")
		auth, err := newAuthResolver().resolveAuth("https://git.example.com/crds.git")
		if auth != nil || err != nil {
			t.Fatalf("unset allowlist must withhold env credentials, got %v, %v", auth, err)
		}
	})

	t.Run("empty allowlist withholds credentials", func(t *testing.T) {
		t.Setenv(envGitCredentialHosts, "  ")
		t.Setenv(envGitUsername, "alice")
		t.Setenv(envGitPassword, "hunter2")
		auth, err := newAuthResolver().resolveAuth("https://git.example.com/crds.git")
		if auth != nil || err != nil {
			t.Fatalf("empty allowlist must withhold env credentials, got %v, %v", auth, err)
		}
	})

	t.Run("host outside the allowlist withholds credentials", func(t *testing.T) {
		t.Setenv(envGitCredentialHosts, "github.com, GitLab.com ")
		t.Setenv(envGitToken, "glpat-tok")
		auth, err := newAuthResolver().resolveAuth("https://attacker.example/repo.git")
		if auth != nil || err != nil {
			t.Fatalf("unlisted host must get no credentials, got %v, %v", auth, err)
		}
	})

	t.Run("allowlist entry is case-insensitive and port-agnostic", func(t *testing.T) {
		t.Setenv(envGitCredentialHosts, " GitHub.COM , gitlab.com")
		t.Setenv(envGitToken, "glpat-tok")
		auth, _ := newAuthResolver().resolveAuth("https://GiThub.com:443/crds.git")
		ba, ok := auth.(*githttp.BasicAuth)
		if !ok {
			t.Fatalf("listed host (any case, default port) must get the token, got %T", auth)
		}
		if ba.Password != "glpat-tok" {
			t.Errorf("token auth = %q/%q, want oauth2/glpat-tok", ba.Username, ba.Password)
		}
	})

	t.Run("withheld host still uses a netrc machine entry", func(t *testing.T) {
		netrc := "machine git.example.com login bob password netrc-pass\n"
		if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte(netrc), 0o600); err != nil {
			t.Fatalf("writing .netrc: %v", err)
		}
		t.Setenv(envGitCredentialHosts, "github.com")
		t.Setenv(envGitToken, "glpat-tok")
		auth, err := newAuthResolver().resolveAuth("https://git.example.com/crds.git")
		if err != nil {
			t.Fatalf("resolveAuth: %v", err)
		}
		ba, ok := auth.(*githttp.BasicAuth)
		if !ok {
			t.Fatalf("want *http.BasicAuth from netrc machine entry, got %T", auth)
		}
		if ba.Username != "bob" || ba.Password != "netrc-pass" {
			t.Errorf("netrc auth = %q/%q, want bob/netrc-pass", ba.Username, ba.Password)
		}
	})

	t.Run("netrc fallback", func(t *testing.T) {
		netrc := "machine git.example.com login bob password netrc-pass\n\nmachine other.example.com login x password y\n"
		if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte(netrc), 0o600); err != nil {
			t.Fatalf("writing .netrc: %v", err)
		}
		auth, err := newAuthResolver().resolveAuth("https://git.example.com:8443/crds.git")
		if err != nil {
			t.Fatalf("resolveAuth: %v", err)
		}
		ba, ok := auth.(*githttp.BasicAuth)
		if !ok {
			t.Fatalf("want *http.BasicAuth, got %T", auth)
		}
		if ba.Username != "bob" || ba.Password != "netrc-pass" {
			t.Errorf("netrc auth = %q/%q, want bob/netrc-pass", ba.Username, ba.Password)
		}
	})

	t.Run("netrc default restricted by the allowlist", func(t *testing.T) {
		netrc := "default login default-user password default-pass\n"
		netrcPath := filepath.Join(home, ".netrc")
		if err := os.WriteFile(netrcPath, []byte(netrc), 0o600); err != nil {
			t.Fatalf("writing .netrc: %v", err)
		}
		defer os.Remove(netrcPath) // keep later subtests credential-free
		t.Run("listed host keeps the default stanza", func(t *testing.T) {
			t.Setenv(envGitCredentialHosts, "git.example.com")
			auth, err := newAuthResolver().resolveAuth("https://git.example.com/crds.git")
			if err != nil {
				t.Fatalf("resolveAuth: %v", err)
			}
			ba, ok := auth.(*githttp.BasicAuth)
			if !ok {
				t.Fatalf("want *http.BasicAuth, got %T", auth)
			}
			if ba.Username != "default-user" || ba.Password != "default-pass" {
				t.Errorf("default stanza auth = %q/%q", ba.Username, ba.Password)
			}
		})
		t.Run("unlisted host does not", func(t *testing.T) {
			t.Setenv(envGitCredentialHosts, "github.com")
			auth, err := newAuthResolver().resolveAuth("https://git.example.com/crds.git")
			if auth != nil || err != nil {
				t.Errorf("default stanza must stay home for unlisted hosts, got %v, %v", auth, err)
			}
		})
		t.Run("unset allowlist keeps the legacy default behavior", func(t *testing.T) {
			auth, err := newAuthResolver().resolveAuth("https://git.example.com/crds.git")
			if err != nil {
				t.Fatalf("resolveAuth: %v", err)
			}
			if auth == nil {
				t.Error("default stanza must keep matching every host while no allowlist is set")
			}
		})
	})

	t.Run("no credentials is nil auth", func(t *testing.T) {
		auth, err := newAuthResolver().resolveAuth("https://public.example.com/crds.git")
		if auth != nil || err != nil {
			t.Errorf("public https must resolve to no auth, got %v, %v", auth, err)
		}
	})
}

// TestResolveAuth_CredsWithheldFailureMsg pins the authFailureMsg remedy of
// a withheld resolution: a transport authentication failure must point at
// FLUXVIEW_GIT_CREDENTIAL_HOSTS, never at the credential values.
func TestResolveAuth_CredsWithheldFailureMsg(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envGitToken, "glpat-tok")

	out, err := newAuthResolver().resolve("https://git.example.com/crds.git")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	msg := out.authFailureMsg(transport.ErrAuthenticationRequired)
	for _, want := range []string{"FLUXVIEW_GIT_CREDENTIAL_HOSTS", "not allowed for this host"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure message %q must mention %q", msg, want)
		}
	}
	if strings.Contains(msg, "glpat-tok") {
		t.Errorf("failure message must not contain the credential: %q", msg)
	}
}

// resolvedSSHAuth resolves auth over an env key and returns the wired
// host key callback for direct invocation, plus the resolution error.
func resolvedSSHAuth(t *testing.T) (ssh.HostKeyCallback, error) {
	t.Helper()
	keyPath := writeKeyFile(t, filepath.Join(t.TempDir(), "id_custom"), mustPriv(t), "")
	t.Setenv(envSSHKey, keyPath)
	auth, err := newAuthResolver().resolveAuth("git@git.example.com:platform/crds.git")
	if err != nil {
		return nil, err
	}
	pk, ok := auth.(*gitssh.PublicKeys)
	if !ok {
		t.Fatalf("want *ssh.PublicKeys, got %T", auth)
	}
	if pk.HostKeyCallback == nil {
		t.Fatal("resolved auth must carry a host key callback")
	}
	return pk.HostKeyCallback, nil
}

func TestHostKeyVerification_Match(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	hostKey, _, _ := genHostKeys(t)
	writeKnownHosts(t, emptyKnownHosts(t), "git.example.com:22", hostKey.PublicKey())

	cb, err := resolvedSSHAuth(t)
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	if err := cb("git.example.com:22", &net.TCPAddr{}, hostKey.PublicKey()); err != nil {
		t.Errorf("matching host key must verify, got: %v", err)
	}
}

func TestHostKeyVerification_UnknownHostFailsNamingFile(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	hostKey, _, _ := genHostKeys(t)
	kh := emptyKnownHosts(t)
	writeKnownHosts(t, kh, "other.example.com:22", hostKey.PublicKey())

	cb, err := resolvedSSHAuth(t)
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	err = cb("git.example.com:22", &net.TCPAddr{}, hostKey.PublicKey())
	if err == nil {
		t.Fatal("an unknown host must fail verification")
	}
	for _, want := range []string{"git.example.com:22", kh, "--git-source-ssh-accept-new"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %q", err, want)
		}
	}
}

func TestHostKeyVerification_ChangedKeyFails(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	hostKey, mitmKey, _ := genHostKeys(t)
	writeKnownHosts(t, emptyKnownHosts(t), "git.example.com:22", hostKey.PublicKey())

	cb, err := resolvedSSHAuth(t)
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	err = cb("git.example.com:22", &net.TCPAddr{}, mitmKey.PublicKey())
	if err == nil || !strings.Contains(err.Error(), "host key changed") {
		t.Errorf("a changed host key must fail loudly, got: %v", err)
	}
}

func TestHostKeyVerification_AcceptNewTOFU(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(EnvSSHAcceptNew, "1")
	hostKey, mitmKey, _ := genHostKeys(t)
	emptyKnownHosts(t) // nothing known: TOFU accepts on first use

	cb, err := resolvedSSHAuth(t)
	if err != nil {
		t.Fatalf("resolveAuth with accept-new: %v", err)
	}
	if err := cb("git.example.com:22", &net.TCPAddr{}, hostKey.PublicKey()); err != nil {
		t.Fatalf("TOFU must accept a new host key, got: %v", err)
	}
	// The accepted key is pinned for the rest of the process: a different
	// key for the same host is a hard failure.
	err = cb("git.example.com:22", &net.TCPAddr{}, mitmKey.PublicKey())
	if err == nil || !strings.Contains(err.Error(), "host key changed") {
		t.Errorf("a re-keyed host must fail even under TOFU, got: %v", err)
	}
}

func TestResolveAuth_NoKnownHostsFile(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	// Point at a path that does not exist: strict mode must refuse.
	t.Setenv(EnvSSHKnownHosts, filepath.Join(t.TempDir(), "absent_known_hosts"))

	_, err := newAuthResolver().resolveAuth("git@git.example.com:crds.git")
	if err == nil || !strings.Contains(err.Error(), EnvSSHKnownHosts) {
		t.Errorf("strict mode without any known_hosts file must fail naming the env var, got: %v", err)
	}

	// TOFU opts in and resolves against an empty in-memory set.
	t.Setenv(EnvSSHAcceptNew, "yes")
	t.Setenv(envSSHKey, writeKeyFile(t, filepath.Join(t.TempDir(), "id_custom"), mustPriv(t), ""))
	if _, err := newAuthResolver().resolveAuth("git@git.example.com:crds.git"); err != nil {
		t.Errorf("accept-new must work without a known_hosts file, got: %v", err)
	}
}

func TestResolveAuth_NoSecretsInErrors(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv(envGitPassword, "pw-leak-canary")
	t.Setenv(envGitToken, "tok-leak-canary")
	t.Setenv(envSSHPassphrase, "pp-leak-canary")

	// No credentials at all.
	t.Setenv(envSSHPassphrase, "pp-leak-canary")
	emptyKnownHosts(t)
	_, err := newAuthResolver().resolveAuth("git@git.example.com:crds.git")
	if err == nil {
		t.Fatal("expected the no-credentials error")
	}
	for _, secret := range []string{"pw-leak-canary", "tok-leak-canary", "pp-leak-canary"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error leaks a secret env value: %v", err)
		}
	}

	// Host key verification failure.
	hostKey, _, _ := genHostKeys(t)
	writeKnownHosts(t, emptyKnownHosts(t), "other.example.com:22", hostKey.PublicKey())
	cb, err := resolvedSSHAuth(t)
	if err != nil {
		t.Fatalf("resolveAuth: %v", err)
	}
	err = cb("git.example.com:22", &net.TCPAddr{}, hostKey.PublicKey())
	if err == nil {
		t.Fatal("expected the unknown-host error")
	}
	for _, secret := range []string{"pw-leak-canary", "tok-leak-canary", "pp-leak-canary"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("host key error leaks a secret env value: %v", err)
		}
	}
}

// genHostKeys generates the key pairs used by host key verification
// tests: the server's advertised key, a different server key (the MITM
// case) and the client identity.
func genHostKeys(t *testing.T) (hostKey, otherKey, clientKey ssh.Signer) {
	t.Helper()
	_, host := genEd25519(t)
	_, other := genEd25519(t)
	_, client := genEd25519(t)
	return host, other, client
}

func mustPriv(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	priv, _ := genEd25519(t)
	return priv
}

func TestNetrcCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".netrc")
	content := `
# comment line
machine git.example.com
login alice
password line-broken

machine other.example.com login bob password other-pass
default
login default-user password default-pass account acct

macdef upload
put something here

machine macro.example.com login mac password mac-pass
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing .netrc: %v", err)
	}

	tests := []struct {
		name      string
		host      string
		allowDef  bool
		wantLogin string
		wantPass  string
		wantOK    bool
	}{
		{"multiline entry", "git.example.com", true, "alice", "line-broken", true},
		{"single line entry", "other.example.com", true, "bob", "other-pass", true},
		{"default fallback", "unknown.example.com", true, "default-user", "default-pass", true},
		{"default disabled", "unknown.example.com", false, "", "", false},
		{"machine entry survives a disabled default", "git.example.com", false, "alice", "line-broken", true},
		{"port is stripped by the caller", "git.example.com", true, "alice", "line-broken", true},
		{"entry after macdef", "macro.example.com", true, "mac", "mac-pass", true},
		{"no default and no match", "", true, "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			login, pass, ok := netrcCredentials(path, tt.host, tt.allowDef)
			if tt.host == "" {
				login, pass, ok = netrcCredentials(filepath.Join(dir, "missing"), "any.example.com", true)
			}
			if ok != tt.wantOK || login != tt.wantLogin || pass != tt.wantPass {
				t.Errorf("netrcCredentials(host=%q) = %q/%q,%v; want %q/%q,%v",
					tt.host, login, pass, ok, tt.wantLogin, tt.wantPass, tt.wantOK)
			}
		})
	}
}

func TestParseNetrcAccountValueIsSkipped(t *testing.T) {
	entries := parseNetrc([]byte("machine h.example.com account acct login carol password p\n"))
	if len(entries) != 1 || entries[0].login != "carol" || entries[0].password != "p" {
		t.Errorf("account token must not eat the login value: %+v", entries)
	}
}
