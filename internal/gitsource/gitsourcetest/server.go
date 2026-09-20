// Package gitsourcetest serves offline git upstreams for tests: an http
// server enforcing basic auth in front of the git CLI's http-backend, and
// an ssh server with publickey auth running git upload-pack over
// gliderlabs/ssh. Both shell out to the git CLI — the same hard
// dependency the CLI-level e2e tests already make — so the fixtures stay
// offline and need no network beyond loopback.
package gitsourcetest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	gliderssh "github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// BareClone makes a bare clone of the working repository at dir (tags and
// branches included) and returns its path, named "<name>.git" under a
// fresh temp directory. Bare layout is what http-backend and upload-pack
// serve directly.
func BareClone(t *testing.T, dir, name string) string {
	t.Helper()
	bare := filepath.Join(t.TempDir(), name+".git")
	if out, err := exec.Command("git", "clone", "--quiet", "--bare", dir, bare).CombinedOutput(); err != nil {
		t.Fatalf("git clone --bare %s: %v\n%s", dir, err, out)
	}
	return bare
}

// HTTPSBasicAuth serves repoRoot over git smart http behind HTTP basic
// auth: only the user:pass pair gets through, anything else receives 401.
// It returns the base URL; a repository cloned with BareClone to
// <repoRoot>/<name>.git is then addressed as <base>/<name>.git.
func HTTPSBasicAuth(t *testing.T, repoRoot, user, pass string) string {
	t.Helper()
	srv := httptest.NewServer(requireBasic(user, pass, gitHTTPBackend(repoRoot)))
	t.Cleanup(srv.Close)
	return srv.URL
}

// requireBasic gates an http.Handler behind HTTP basic auth.
func requireBasic(user, pass string, next http.Handler) http.Handler {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != want {
			w.Header().Set("WWW-Authenticate", `Basic realm="fluxview-test"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// gitHTTPBackend answers git smart-http requests by running the git CLI's
// http-backend CGI against repoRoot and translating its CGI response.
func gitHTTPBackend(repoRoot string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cmd := exec.Command("git", "http-backend")
		cmd.Env = append(os.Environ(),
			"GIT_PROJECT_ROOT="+repoRoot,
			"GIT_HTTP_EXPORT_ALL=1",
			"REQUEST_METHOD="+r.Method,
			"PATH_INFO="+r.URL.Path,
			"QUERY_STRING="+r.URL.RawQuery,
			"CONTENT_TYPE="+r.Header.Get("Content-Type"),
			"REMOTE_ADDR="+r.RemoteAddr,
		)
		if cl := r.Header.Get("Content-Length"); cl != "" {
			cmd.Env = append(cmd.Env, "CONTENT_LENGTH="+cl)
		}
		cmd.Stdin = r.Body
		out, err := cmd.Output()
		if err != nil {
			http.Error(w, "git http-backend: "+err.Error(), http.StatusInternalServerError)
			return
		}
		head, body, _ := strings.Cut(string(out), "\r\n\r\n")
		status := http.StatusOK
		for _, line := range strings.Split(head, "\r\n") {
			key, value, ok := strings.Cut(line, ": ")
			if !ok {
				continue
			}
			if strings.EqualFold(key, "Status") {
				if fields := strings.Fields(value); len(fields) > 0 {
					if code, err := strconv.Atoi(fields[0]); err == nil {
						status = code
					}
				}
				continue
			}
			w.Header().Set(key, value)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

// SSHGitServer serves git over ssh with publickey auth: only
// authorizedKey may connect, every other key fails the handshake. It
// returns the base URL (ssh://git@host:port — repositories are addressed
// by absolute path: <base><abs-repo-path>) and the server's host key for
// the client's known_hosts.
func SSHGitServer(t *testing.T, authorizedKey gossh.PublicKey) (base string, hostKey gossh.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating host key: %v", err)
	}
	hostSigner, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("host key signer: %v", err)
	}

	srv := &gliderssh.Server{
		Handler: serveUploadPack,
		PublicKeyHandler: func(ctx gliderssh.Context, key gliderssh.PublicKey) bool {
			return bytes.Equal(key.Marshal(), authorizedKey.Marshal())
		},
	}
	srv.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return fmt.Sprintf("ssh://git@%s", ln.Addr().String()), hostSigner
}

// serveUploadPack pipes an ssh session into `git upload-pack` — the git
// server side of clone/fetch/ls-remote. The command the client runs is
// "git-upload-pack '<path>'" with an absolute repository path.
func serveUploadPack(s gliderssh.Session) {
	args := s.Command()
	if len(args) != 2 || args[0] != "git-upload-pack" {
		fmt.Fprintf(s.Stderr(), "unsupported command %v\n", args)
		_ = s.Exit(1)
		return
	}

	cmd := exec.Command("git", "upload-pack", args[1])
	stdin, err := cmd.StdinPipe()
	if err != nil {
		fmt.Fprintf(s.Stderr(), "stdin pipe: %v\n", err)
		_ = s.Exit(1)
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(s.Stderr(), "stdout pipe: %v\n", err)
		_ = s.Exit(1)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(s.Stderr(), "stderr pipe: %v\n", err)
		_ = s.Exit(1)
		return
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(s.Stderr(), "starting upload-pack: %v\n", err)
		_ = s.Exit(1)
		return
	}

	go func() {
		defer stdin.Close()
		_, _ = io.Copy(stdin, s)
	}()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(s.Stderr(), stderr)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(s, stdout)
	}()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		_ = s.Exit(1)
		return
	}
}
