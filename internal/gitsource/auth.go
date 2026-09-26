// Authentication of external git sources from the machine environment —
// the same model as native git on a developer box or CI runner: SSH keys
// (env file, ssh-agent, default identity files) for ssh:// and scp-style
// URLs, basic auth from env pairs or ~/.netrc for http(s)://. Env and
// netrc-default credentials apply only to hosts listed in
// FLUXVIEW_GIT_CREDENTIAL_HOSTS: a URL from a manifest is untrusted input,
// so a token from CI must never travel to a host the user did not name.
// Manifests stay untouched: spec.secretRef is ignored (no cluster access
// by design) and credentials embedded in URLs are ignored (NormalizeGitURL
// strips them; the source of credentials is the environment only).

package gitsource

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/skeema/knownhosts"
	sshagent "github.com/xanzy/ssh-agent"
	gossh "golang.org/x/crypto/ssh"
)

// Environment variables recognized by the auth resolver. Credentials are
// env-only on purpose (SSL_CERT_FILE convention): secrets on a command
// line leak into ps output and CI logs. The two policy knobs marked with
// flags (--git-source-ssh-known-hosts, --git-source-ssh-accept-new) are
// exported for the CLI wiring; the credential variables never get flags.
const (
	envSSHKey        = "FLUXVIEW_GIT_SSH_KEY"
	envSSHPassphrase = "FLUXVIEW_GIT_SSH_PASSPHRASE"
	// EnvSSHKnownHosts names the known_hosts file; empty means the
	// standard set (~/.ssh/known_hosts + /etc/ssh/ssh_known_hosts).
	EnvSSHKnownHosts = "FLUXVIEW_GIT_SSH_KNOWN_HOSTS"
	// EnvSSHAcceptNew opts into accept-new (TOFU) host key verification.
	EnvSSHAcceptNew = "FLUXVIEW_GIT_SSH_ACCEPT_NEW"
	envGitUsername  = "FLUXVIEW_GIT_USERNAME"
	envGitPassword  = "FLUXVIEW_GIT_PASSWORD"
	envGitToken     = "FLUXVIEW_GIT_TOKEN"
	// envGitCredentialHosts names the CSV list of hosts the env and
	// netrc-default credentials may travel to; empty means no host at all
	// (fail-closed). Like the credential variables it never gets a flag.
	envGitCredentialHosts = "FLUXVIEW_GIT_CREDENTIAL_HOSTS"
)

// DefaultSSHKnownHostsFile returns the known_hosts file for SSH host key
// verification of external git sources: env FLUXVIEW_GIT_SSH_KNOWN_HOSTS,
// else "" — the standard set of ~/.ssh/known_hosts plus
// /etc/ssh/ssh_known_hosts.
func DefaultSSHKnownHostsFile() string {
	return os.Getenv(EnvSSHKnownHosts)
}

// DefaultSSHAcceptNew reports whether accept-new (TOFU) host key
// verification is on by default: env FLUXVIEW_GIT_SSH_ACCEPT_NEW.
func DefaultSSHAcceptNew() bool {
	return acceptNew()
}

// defaultSSHUser is the SSH user for git remotes when the URL spells none.
const defaultSSHUser = "git"

// defaultIdentityFiles are the conventional ssh key files tried, in order,
// when neither FLUXVIEW_GIT_SSH_KEY nor a live ssh-agent provides a key.
var defaultIdentityFiles = []string{"id_ed25519", "id_ecdsa", "id_rsa"}

// gitEndpoint is the auth-relevant decomposition of a git URL.
type gitEndpoint struct {
	// local marks URLs that never authenticate: file://, plain paths.
	local bool
	// scp marks the [user@]host:path spelling of the ssh transport.
	scp bool
	// scheme is the lowercased URL scheme ("" for scp and local paths).
	scheme string
	// user is the ssh user spelled in the URL (scp or ssh://userinfo),
	// empty when the URL names none.
	user string
	// host is host[:port], lowercased; empty for local URLs.
	host string
}

// parseGitEndpoint splits a git URL the same way NormalizeGitURL decides
// scp-form vs local path, so auth decisions and cache identity always
// agree on what a URL is.
func parseGitEndpoint(raw string) gitEndpoint {
	s := strings.TrimSpace(raw)
	if s == "" {
		return gitEndpoint{local: true}
	}
	if strings.HasPrefix(s, "file://") || strings.HasPrefix(s, "file:") {
		return gitEndpoint{local: true, scheme: "file"}
	}
	if i := strings.Index(s, "://"); i >= 0 {
		scheme := strings.ToLower(s[:i])
		rest := s[i+3:]
		user := ""
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			user, rest = rest[:at], rest[at+1:]
		}
		host := rest
		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			host = rest[:slash]
		}
		if scheme == "http" || scheme == "https" {
			// Credentials in URLs are ignored by design; the source of
			// basic auth is the environment only.
			user = ""
		}
		return gitEndpoint{scheme: scheme, user: user, host: strings.ToLower(host)}
	}
	// scp-style [user@]host:path — the colon must precede any slash, the
	// same rule NormalizeGitURL applies.
	user := ""
	if at := strings.LastIndex(s, "@"); at >= 0 {
		user, s = s[:at], s[at+1:]
	}
	if colon := strings.IndexByte(s, ':'); colon > 0 {
		if slash := strings.IndexByte(s, '/'); slash < 0 || colon < slash {
			return gitEndpoint{scp: true, user: user, host: strings.ToLower(s[:colon])}
		}
	}
	return gitEndpoint{local: true}
}

// hostWithPort returns the host with an explicit default port, matching
// how go-git addresses ssh remotes (and spells them to host key
// callbacks).
func (e gitEndpoint) hostWithPort() string {
	h := e.host
	if h == "" {
		return h
	}
	if colon := strings.LastIndexByte(h, ':'); colon > strings.LastIndexByte(h, ']') {
		return h
	}
	return h + ":22"
}

// authMapKey is the memoization key of a resolution: transport family
// plus host. The ssh:// and scp spellings share one family; http and https
// stay apart from it (and from each other) so a resolved credential of
// one transport never surfaces as the auth of a clone over another —
// an http.BasicAuth cannot authenticate ssh, and an ssh dead end must
// not fail https of the same host.
func (e gitEndpoint) authMapKey() string {
	if e.scheme == "ssh" || e.scp {
		return "ssh://" + e.hostWithPort()
	}
	return e.scheme + "://" + e.host
}

// hostname strips the port from a host[:port] string.
func hostname(hostPort string) string {
	if colon := strings.LastIndexByte(hostPort, ':'); colon > strings.LastIndexByte(hostPort, ']') {
		return hostPort[:colon]
	}
	return hostPort
}

// authOutcome is one memoized host resolution: the auth method (nil = no
// auth needed) or the resolution error, remembered so a broken
// configuration fails identically on every clone of that host. The ssh
// flag and keySource feed the transport-failure classification below.
type authOutcome struct {
	auth transport.AuthMethod
	err  error
	// ssh marks ssh-transport resolutions, whose connect failures may mean
	// rejected credentials.
	ssh bool
	// keySource names the credential source in play ("FLUXVIEW_GIT_SSH_KEY",
	// "ssh-agent", "~/.ssh/id_ed25519"), for error messages.
	keySource string
	// credsWithheld marks http resolutions where environment credentials
	// exist but the host is outside FLUXVIEW_GIT_CREDENTIAL_HOSTS — no auth
	// is returned, and transport failures name the allowlist as the remedy.
	credsWithheld bool
}

// authFailureMsg explains a transport-level error that looks like rejected
// credentials; empty when the error is something else (network trouble,
// host key verification — those messages already stand for themselves).
// Secret values never appear: only the credential source is named.
func (o authOutcome) authFailureMsg(err error) string {
	if err == nil || o.err != nil {
		return ""
	}
	if o.ssh {
		if strings.Contains(err.Error(), "unable to authenticate") {
			return fmt.Sprintf("SSH authentication failed: the server rejected the %s credentials", o.keySource)
		}
		return ""
	}
	if errors.Is(err, transport.ErrAuthenticationRequired) ||
		errors.Is(err, transport.ErrAuthorizationFailed) ||
		errors.Is(err, transport.ErrInvalidAuthMethod) {
		if o.credsWithheld {
			return "private repository or bad credentials (FLUXVIEW_GIT_USERNAME/FLUXVIEW_GIT_TOKEN are set but not allowed for this host; add it to FLUXVIEW_GIT_CREDENTIAL_HOSTS)"
		}
		return "private repository or bad credentials (set FLUXVIEW_GIT_USERNAME/FLUXVIEW_GIT_PASSWORD or ~/.netrc)"
	}
	return ""
}

// authResolver resolves transport.AuthMethod for git URLs from the machine
// environment. One host resolves once per process (memoized by
// host and transport family); the ssh-agent connection, when used, is closed by Close.
// Construct with newAuthResolver; safe for concurrent use.
type authResolver struct {
	// mu guards perHost.
	mu      sync.Mutex
	perHost map[string]authOutcome

	// agentMu guards the lazy ssh-agent dial.
	agentMu     sync.Mutex
	agentAuth   *gitssh.PublicKeysCallback
	agentConn   io.Closer
	agentDialed bool

	// hostsMu guards the one-time host key callback build.
	hostsMu  sync.Mutex
	hostsCB  gossh.HostKeyCallback
	hostsErr error

	// tofuMu guards tofuMap.
	tofuMu  sync.Mutex
	tofuMap map[string]string // hostWithPort → sha256 fingerprint accepted by TOFU
}

func newAuthResolver() *authResolver {
	return &authResolver{
		perHost: make(map[string]authOutcome),
		tofuMap: make(map[string]string),
	}
}

// Close releases the resources held by resolved auth — the ssh-agent
// connection, if any — and drops memoized resolutions with it. Call from
// the Fetcher close path; safe to call more than once.
func (r *authResolver) Close() error {
	r.agentMu.Lock()
	conn := r.agentConn
	r.agentConn = nil
	r.agentAuth = nil
	r.agentDialed = false
	r.agentMu.Unlock()

	r.mu.Lock()
	r.perHost = make(map[string]authOutcome)
	r.mu.Unlock()

	if conn != nil {
		return conn.Close()
	}
	return nil
}

// resolveAuth returns the transport auth for a git URL, or nil when the
// URL needs none (local paths, git://, public http(s) repositories).
// Resolution is memoized per transport family and host.
func (r *authResolver) resolveAuth(url string) (transport.AuthMethod, error) {
	out, err := r.resolve(url)
	return out.auth, err
}

// resolve is the full-form resolution used by the fetch path, which also
// needs the failure classification data of the outcome.
func (r *authResolver) resolve(url string) (authOutcome, error) {
	ep := parseGitEndpoint(url)
	switch {
	case ep.local || ep.host == "":
		return authOutcome{}, nil
	case ep.scheme == "http" || ep.scheme == "https":
		return r.resolvePerHost(ep, r.resolveHTTPAuth)
	case ep.scheme == "ssh" || ep.scp:
		return r.resolvePerHost(ep, r.resolveSSHAuth)
	default:
		// git:// and exotic schemes carry no auth (the git protocol has
		// no authentication at all).
		return authOutcome{}, nil
	}
}

// resolvePerHost memoizes one resolve function's outcome per transport
// family and host (authMapKey).
func (r *authResolver) resolvePerHost(ep gitEndpoint, resolve func(gitEndpoint) (authOutcome, error)) (authOutcome, error) {
	key := ep.authMapKey()

	r.mu.Lock()
	out, ok := r.perHost[key]
	r.mu.Unlock()
	if ok {
		return out, out.err
	}

	out, err := resolve(ep)

	r.mu.Lock()
	r.perHost[key] = out
	r.mu.Unlock()
	return out, err
}

// credentialHostAllowed reports whether host (a bare hostname, no port)
// is listed in FLUXVIEW_GIT_CREDENTIAL_HOSTS. The list is CSV, entries are
// whitespace-trimmed and matched case-insensitively. An empty or unset
// list allows nothing: fail-closed, so a URL from an untrusted manifest
// (a fork redirecting to attacker.example) can never harvest a CI token.
func credentialHostAllowed(host string) bool {
	list := os.Getenv(envGitCredentialHosts)
	if strings.TrimSpace(list) == "" {
		return false
	}
	host = strings.ToLower(strings.TrimSpace(host))
	for _, entry := range strings.Split(list, ",") {
		if strings.ToLower(strings.TrimSpace(entry)) == host {
			return true
		}
	}
	return false
}

// credentialHostsSet reports whether FLUXVIEW_GIT_CREDENTIAL_HOSTS names
// any host at all; when it does not, the netrc default stanza keeps its
// legacy match-everything behavior.
func credentialHostsSet() bool {
	return strings.TrimSpace(os.Getenv(envGitCredentialHosts)) != ""
}

// resolveHTTPAuth resolves basic auth for an http(s) endpoint: the env
// pair has priority, a forge token is sugar for oauth2:<token>, ~/.netrc
// (parsed natively — go-git has no netrc support of its own) is the
// fallback, and public repositories resolve to no auth at all. The env
// pair and the token travel only to allowlisted hosts; a netrc default
// stanza is bound by the same list once it is set (machine stanzas are
// scoped by their own host entry and never need the list).
func (r *authResolver) resolveHTTPAuth(ep gitEndpoint) (authOutcome, error) {
	host := hostname(ep.host)
	allowed := credentialHostAllowed(host)
	u, p := os.Getenv(envGitUsername), os.Getenv(envGitPassword)
	tok := os.Getenv(envGitToken)
	if allowed {
		if u != "" && p != "" {
			return authOutcome{auth: &githttp.BasicAuth{Username: u, Password: p}, keySource: envGitUsername}, nil
		}
		if tok != "" {
			// GitLab/GitHub PATs authenticate over https as oauth2:<token>.
			return authOutcome{auth: &githttp.BasicAuth{Username: "oauth2", Password: tok}, keySource: envGitToken}, nil
		}
	}
	// Machine stanzas of ~/.netrc are scoped by their own host and need no
	// allowlist; the catch-all default stanza obeys the list once set.
	if login, pass, ok := netrcCredentials(netrcPath(), host, !credentialHostsSet() || allowed); ok {
		return authOutcome{auth: &githttp.BasicAuth{Username: login, Password: pass}, keySource: "~/.netrc"}, nil
	}
	if (u != "" && p != "") || tok != "" {
		// Credentials exist but this host is not on the allowlist and no
		// scoped netrc entry covers it: they stay home, and a 401 names
		// the remedy.
		return authOutcome{credsWithheld: true}, nil
	}
	return authOutcome{}, nil
}

// resolveSSHAuth resolves auth for an ssh (ssh:// or scp-style) endpoint:
// host key verification is always wired (strict known_hosts, or TOFU when
// opted in), user comes from the URL with a git default, and the key is
// the first available of FLUXVIEW_GIT_SSH_KEY, a live ssh-agent, and the
// default identity files.
func (r *authResolver) resolveSSHAuth(ep gitEndpoint) (authOutcome, error) {
	cb, err := r.hostKeyCallback()
	if err != nil {
		return authOutcome{ssh: true, err: err}, err
	}

	user := ep.user
	if user == "" {
		user = defaultSSHUser
	}
	pass := os.Getenv(envSSHPassphrase)

	if path := os.Getenv(envSSHKey); path != "" {
		auth, err := gitssh.NewPublicKeysFromFile(user, path, pass)
		if err != nil {
			err = fmt.Errorf("reading SSH key %s (set by FLUXVIEW_GIT_SSH_KEY): %w%s", path, err, passphraseHint(pass))
			return authOutcome{ssh: true, keySource: envSSHKey, err: err}, err
		}
		auth.HostKeyCallback = cb
		return authOutcome{auth: auth, ssh: true, keySource: envSSHKey}, nil
	}

	if agentAuth := r.dialAgent(); agentAuth != nil {
		auth := *agentAuth
		auth.User = user
		auth.HostKeyCallback = cb
		return authOutcome{auth: &auth, ssh: true, keySource: "ssh-agent"}, nil
	}

	for _, name := range defaultIdentityFiles {
		path := filepath.Join(sshDir(), name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		auth, err := gitssh.NewPublicKeysFromFile(user, path, pass)
		if err != nil {
			err = fmt.Errorf("reading SSH identity %s: %w%s", path, err, passphraseHint(pass))
			return authOutcome{ssh: true, keySource: "~/.ssh/" + name, err: err}, err
		}
		auth.HostKeyCallback = cb
		return authOutcome{auth: auth, ssh: true, keySource: "~/.ssh/" + name}, nil
	}

	err = fmt.Errorf(
		"no SSH credentials found for %s@%s (set FLUXVIEW_GIT_SSH_KEY, enable ssh-agent via SSH_AUTH_SOCK, or provide ~/.ssh/id_ed25519, id_ecdsa or id_rsa)",
		user, ep.hostWithPort())
	return authOutcome{ssh: true, err: err}, err
}

// dialAgent lazily dials the ssh-agent named by SSH_AUTH_SOCK and returns
// an agent-backed auth template (user and host key callback are filled in
// per host). A missing, dead or keyless agent is not an error: resolution
// falls through to the default identity files, like native ssh.
func (r *authResolver) dialAgent() *gitssh.PublicKeysCallback {
	r.agentMu.Lock()
	defer r.agentMu.Unlock()
	if !r.agentDialed {
		r.agentDialed = true
		if os.Getenv("SSH_AUTH_SOCK") != "" {
			if a, conn, err := sshagent.New(); err == nil {
				// An agent holding no keys cannot authenticate anything —
				// fall through to the identity files instead of failing
				// later with an empty offer.
				if signers, err := a.Signers(); err == nil && len(signers) > 0 {
					r.agentAuth = &gitssh.PublicKeysCallback{Callback: a.Signers}
					r.agentConn = conn
				} else {
					conn.Close()
				}
			}
		}
	}
	return r.agentAuth
}

// hostKeyCallback builds (once per resolver) the host key verification
// callback: strict known_hosts by default, accept-new TOFU when opted in
// through FLUXVIEW_GIT_SSH_ACCEPT_NEW. Keys accepted by TOFU are
// remembered in memory only — known_hosts files are never written,
// keeping CI runs deterministic.
func (r *authResolver) hostKeyCallback() (gossh.HostKeyCallback, error) {
	r.hostsMu.Lock()
	defer r.hostsMu.Unlock()
	if r.hostsCB != nil || r.hostsErr != nil {
		return r.hostsCB, r.hostsErr
	}

	files := knownHostsFiles()
	existing := existingFiles(files)
	accept := acceptNew()
	if len(existing) == 0 && !accept {
		r.hostsErr = fmt.Errorf(
			"no known_hosts file found (looked at %s; set FLUXVIEW_GIT_SSH_KNOWN_HOSTS, create one, or set FLUXVIEW_GIT_SSH_ACCEPT_NEW=1 for TOFU)",
			strings.Join(files, ", "))
		return nil, r.hostsErr
	}
	db, err := knownhosts.NewDB(existing...)
	if err != nil {
		r.hostsErr = fmt.Errorf("reading known_hosts %s: %w", strings.Join(existing, ", "), err)
		return nil, r.hostsErr
	}

	filesDesc := strings.Join(existing, ", ")
	strict := db.HostKeyCallback()
	r.hostsCB = func(hostWithPort string, remote net.Addr, key gossh.PublicKey) error {
		err := strict(hostWithPort, remote, key)
		switch {
		case err == nil:
			return nil
		case knownhosts.IsHostKeyChanged(err):
			return fmt.Errorf(
				"host key verification failed for %s: host key changed (possible man-in-the-middle); see %s",
				hostWithPort, filesDesc)
		case knownhosts.IsHostUnknown(err) && accept:
			return r.tofuRemember(hostWithPort, key)
		case knownhosts.IsHostUnknown(err):
			return fmt.Errorf(
				"host key verification failed for %s: host not in %s (pass --git-source-ssh-accept-new or set FLUXVIEW_GIT_SSH_ACCEPT_NEW=1 for TOFU)",
				hostWithPort, filesDesc)
		default:
			return fmt.Errorf("host key verification failed for %s: %w", hostWithPort, err)
		}
	}
	return r.hostsCB, nil
}

// tofuRemember accepts a host key on first use and pins it for the rest
// of the process: a later presentation of a different key for the same
// host is a hard failure, like OpenSSH's accept-new.
func (r *authResolver) tofuRemember(hostWithPort string, key gossh.PublicKey) error {
	fp := gossh.FingerprintSHA256(key)
	r.tofuMu.Lock()
	defer r.tofuMu.Unlock()
	if prev, ok := r.tofuMap[hostWithPort]; ok {
		if prev != fp {
			return fmt.Errorf(
				"host key verification failed for %s: host key changed (possible man-in-the-middle); a different key was accepted earlier in this run",
				hostWithPort)
		}
		return nil
	}
	r.tofuMap[hostWithPort] = fp
	return nil
}

// knownHostsFiles lists the known_hosts files to verify SSH host keys
// against: FLUXVIEW_GIT_SSH_KNOWN_HOSTS, else the user's and the system
// file (the OpenSSH defaults).
func knownHostsFiles() []string {
	if f := os.Getenv(EnvSSHKnownHosts); f != "" {
		return []string{f}
	}
	files := []string{}
	if home, err := os.UserHomeDir(); err == nil {
		files = append(files, filepath.Join(home, ".ssh", "known_hosts"))
	}
	return append(files, "/etc/ssh/ssh_known_hosts")
}

// acceptNew reports whether TOFU (accept-new) host key verification is
// opted in through FLUXVIEW_GIT_SSH_ACCEPT_NEW.
func acceptNew() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(EnvSSHAcceptNew))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// sshDir returns the user's ~/.ssh directory (empty string when the home
// directory cannot be determined).
func sshDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ssh")
}

// passphraseHint suggests the passphrase env for an unparseable key when
// no passphrase was configured.
func passphraseHint(pass string) string {
	if pass != "" {
		return ""
	}
	return " (set FLUXVIEW_GIT_SSH_PASSPHRASE if the key is encrypted)"
}

// existingFiles filters paths down to the ones that stat cleanly.
func existingFiles(paths []string) []string {
	var out []string
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// netrcPath returns the netrc file location (~/.netrc), or "" when the
// home directory cannot be determined.
func netrcPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".netrc")
}

// netrcEntry is one machine (or default) stanza of a netrc file.
type netrcEntry struct {
	machine   string
	isDefault bool
	login     string
	password  string
}

// parseNetrc extracts the machine entries of a netrc file: a pragmatic
// reader of the classic format — machine/default stanzas with
// login/password pairs, comments and macdef macro blocks skipped.
func parseNetrc(data []byte) []netrcEntry {
	var entries []netrcEntry
	inMacro := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if inMacro {
			if line == "" {
				inMacro = false
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		toks := strings.Fields(line)
		for i := 0; i < len(toks); i++ {
			switch toks[i] {
			case "macdef":
				inMacro = true
			case "default":
				entries = append(entries, netrcEntry{isDefault: true})
			case "machine", "login", "password", "account":
				key := toks[i]
				i++
				if i >= len(toks) {
					continue // value missing — ignore the pair
				}
				switch key {
				case "machine":
					entries = append(entries, netrcEntry{machine: strings.ToLower(toks[i])})
				case "login":
					if n := len(entries); n > 0 {
						entries[n-1].login = toks[i]
					}
				case "password":
					if n := len(entries); n > 0 {
						entries[n-1].password = toks[i]
					}
				}
			}
			if inMacro {
				break // the rest of the line is a macro body
			}
		}
	}
	return entries
}

// netrcCredentials returns the login/password pair for host from the netrc
// file at path, if it has one: the first matching machine entry wins, a
// default entry is the fallback — unless allowDefault is false, used when
// FLUXVIEW_GIT_CREDENTIAL_HOSTS restricts where credentials may travel.
// Matching is by hostname without port — the netrc format has no port
// field. A missing or unreadable file yields no credentials: go-git has no
// netrc support of its own, so fluxview parses the file here and stays
// silent about its absence.
func netrcCredentials(path, host string, allowDefault bool) (login, password string, ok bool) {
	if path == "" {
		return "", "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", false
	}
	host = strings.ToLower(host)
	defIdx := -1
	entries := parseNetrc(data)
	for i := range entries {
		e := &entries[i]
		if e.login == "" || e.password == "" {
			continue
		}
		if e.machine == host {
			return e.login, e.password, true
		}
		if e.isDefault && defIdx < 0 {
			defIdx = i
		}
	}
	if allowDefault && defIdx >= 0 {
		return entries[defIdx].login, entries[defIdx].password, true
	}
	return "", "", false
}
