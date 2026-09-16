// Package gitsource fetches external Flux GitRepository sources into an
// on-disk cache of clones — a minimal source-controller analog used by the
// Kustomization pipeline to build paths that live outside the cluster
// repository.
//
// Cache layout (content-addressed, every directory immutable once written):
//
//	<cacheDir>/data/<sha256(url + "\x00" + resolved)>/   full clone at the
//	                                                    resolved ref, plus a
//	                                                    .fluxview-source.json seed
//	<cacheDir>/refs/<sha256(url + "\x00" + ref)>.json   floating-ref pointer
//	<cacheDir>/tmp/                                     in-progress clones
//
// resolved is the immutable form of the requested ref — "commit:<sha>" or
// "tag:<tag>" — so a clone directory never needs refreshing: a moved branch
// or a new semver winner resolves to a different directory, and the old one
// simply stays until the cache is cleared by hand (no eviction, same policy
// as the other fluxview caches).
//
// Pinned refs (commit, tag) map straight to their directory; floating refs
// (branch, semver, no ref — HEAD) go through a small pointer file that
// caches the resolution for the TTL. Both clone directories and pointer
// files appear atomically (clone or write to a temp name, then rename), so
// concurrent fluxview processes sharing one cache never observe a partial
// directory: a rename race loses to whichever process got there first, and
// the loser reuses the winner's directory.
package gitsource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/git"
)

// seedFileName records the provenance of one clone directory inside the
// clone itself; its presence and integrity mark the directory as complete.
const seedFileName = ".fluxview-source.json"

// DefaultCacheDir returns the git source cache directory: env
// FLUXVIEW_GIT_SOURCE_CACHE_DIR, else a sibling of the other fluxview
// caches under ~/.cache/fluxview. The values "off"/"none"/"disabled"
// disable the cache (checked in NewFetcher).
func DefaultCacheDir() string {
	if dir := os.Getenv("FLUXVIEW_GIT_SOURCE_CACHE_DIR"); dir != "" {
		return dir
	}
	if base := os.Getenv("XDG_CACHE_HOME"); base != "" {
		return filepath.Join(base, "fluxview", "git-sources")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "fluxview-git-sources")
	}
	return filepath.Join(home, ".cache", "fluxview", "git-sources")
}

// warnTTLOnce keeps the invalid-env warning to a single line per process
// (same convention as the Helm and build caches).
var warnTTLOnce sync.Once

// DefaultTTL returns how long a floating-ref resolution (branch/semver/HEAD
// pointer) stays fresh: env FLUXVIEW_GIT_SOURCE_CACHE_TTL (Go duration),
// else 10 minutes. Zero always re-resolves (fresh results are still written).
func DefaultTTL() time.Duration {
	const def = 10 * time.Minute
	v := os.Getenv("FLUXVIEW_GIT_SOURCE_CACHE_TTL")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		warnTTLOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "Warning: invalid FLUXVIEW_GIT_SOURCE_CACHE_TTL %q, using %s\n", v, def)
		})
		return def
	}
	return d
}

// cacheDisabled reports whether a cache directory flag value turns the
// cache off — the same disable spell as every other fluxview cache.
func cacheDisabled(dir string) bool {
	switch strings.ToLower(dir) {
	case "off", "none", "disabled":
		return true
	}
	return false
}

// meta is the shared shape of the in-clone seed file and the floating-ref
// pointer: what was requested, what it resolved to, and when.
type meta struct {
	URL       string `json:"url"`
	Ref       string `json:"ref"`
	Resolved  string `json:"resolved"`
	FetchedAt string `json:"fetchedAt"`
}

// Fetcher clones external GitRepository sources into the git source cache.
// A nil *Fetcher (or a disabled cache directory) still fetches — every
// clone lands in a private temp directory with no reuse — because the
// Kustomization pipeline must not silently skip external resources. Those
// temp clones accumulate in one per-Fetcher directory removed by Close.
type Fetcher struct {
	// dir is the cache directory; empty means "no reuse" (temp clones).
	dir string
	// ttl is how long a floating-ref resolution stays fresh. Zero always
	// re-resolves. Pinned refs ignore it.
	ttl time.Duration

	// offDir is the base directory for cache-disabled clones, created
	// lazily and removed by Close (guarded by mu).
	offDir string

	// inflight dedups concurrent Ensure calls for the same clone directory
	// within one process: the second caller waits for the first clone
	// instead of racing it with a duplicate.
	mu       sync.Mutex
	inflight map[string]chan struct{}
}

// NewFetcher creates a Fetcher over the given cache directory and TTL.
// A dir of "off"/"none"/"disabled" (or empty) disables clone reuse.
// A negative ttl is normalized to zero (always re-resolve floating refs).
func NewFetcher(cacheDir string, ttl time.Duration) *Fetcher {
	f := &Fetcher{inflight: make(map[string]chan struct{})}
	if !cacheDisabled(cacheDir) {
		f.dir = cacheDir
	}
	if ttl < 0 {
		ttl = 0
	}
	f.ttl = ttl
	return f
}

// Close removes the temporary clones made while the cache was disabled.
// Cached clones (the data/ tree) are kept — they are the point of the
// cache. Safe on a nil Fetcher and safe to call twice.
func (f *Fetcher) Close() error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	offDir := f.offDir
	f.mu.Unlock()
	if offDir == "" {
		return nil
	}
	return os.RemoveAll(offDir)
}

// offTempDir returns the base directory for cache-disabled clones, creating
// it on first use.
func (f *Fetcher) offTempDir() (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.offDir == "" {
		dir, err := os.MkdirTemp("", "fluxview-gitsource-*")
		if err != nil {
			return "", fmt.Errorf("preparing git source clone: %w", err)
		}
		f.offDir = dir
	}
	return f.offDir, nil
}

// Ensure returns a local directory holding the repository clone at the ref
// requested by repo (spec.ref resolved per Flux priority: commit > tag >
// branch > semver > HEAD), cloning it into the cache when needed. The
// directory is immutable for a given (url, resolved ref) pair; callers may
// read from it freely but must not modify it. A nil Fetcher fetches to a
// private temp directory with no reuse (removed by Close).
func (f *Fetcher) Ensure(ctx context.Context, repo flux.GitRepository) (string, error) {
	url := strings.TrimSpace(repo.Spec.URL)
	if url == "" {
		return "", fmt.Errorf("GitRepository %s/%s has no spec.url",
			repo.Metadata.Namespace, repo.Metadata.Name)
	}

	refSpec := repo.Spec.Ref.RefString()
	pinned := repo.Spec.Ref.IsPinned()

	// Resolution: pinned refs are their own resolved form; floating refs go
	// through the pointer (network only when it is missing or expired).
	// cloneRef names the remote ref to clone shallowly when the resolved
	// form alone would force a full clone (a resolved commit sha from a
	// branch/HEAD is the branch tip — cloning the tip by name is the same
	// tree, but shallow).
	resolved := refSpec
	cloneRef := ""
	resolvedFresh := false
	if !pinned {
		if f != nil && f.dir != "" {
			if p, ok := readPointer(f.dir, cacheKey(url, refSpec)); ok && f.fresh(p) {
				resolved = p.Resolved
				resolvedFresh = true
			}
		}
		if !resolvedFresh {
			r, ref, err := resolveFloating(ctx, url, repo.Spec.Ref)
			if err != nil {
				return "", err
			}
			resolved, cloneRef = r, ref
		}
	}

	// Clone directory: content-addressed by (url, resolved). Without the
	// cache every Ensure gets a private temp clone — no reuse at all, all
	// removed by Close.
	if f == nil || f.dir == "" {
		var base string
		if f != nil {
			var err error
			if base, err = f.offTempDir(); err != nil {
				return "", err
			}
		}
		dst, err := os.MkdirTemp(base, "clone-*")
		if err != nil {
			return "", fmt.Errorf("preparing git source clone: %w", err)
		}
		return cloneOnce(ctx, url, resolved, cloneRef, dst)
	}

	dir := filepath.Join(f.dir, "data", cacheKey(url, resolved))
	if seedValid(dir) {
		f.rememberPointer(url, refSpec, resolved, resolvedFresh)
		return dir, nil
	}

	if err := f.cloneSynced(ctx, url, resolved, cloneRef, dir); err != nil {
		return "", err
	}
	f.rememberPointer(url, refSpec, resolved, resolvedFresh)
	return dir, nil
}

// cloneSynced clones url at resolved into dir, deduplicating concurrent
// clones of the same directory within this process.
func (f *Fetcher) cloneSynced(ctx context.Context, url, resolved, cloneRef, dir string) error {
	key := cacheKey(url, resolved)

	f.mu.Lock()
	if done, ok := f.inflight[key]; ok {
		f.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
		if seedValid(dir) {
			return nil
		}
		// The first clone failed; try again ourselves.
		return f.cloneSynced(ctx, url, resolved, cloneRef, dir)
	}
	done := make(chan struct{})
	f.inflight[key] = done
	f.mu.Unlock()

	err := cloneAtomic(ctx, f.dir, url, resolved, cloneRef, dir)

	f.mu.Lock()
	delete(f.inflight, key)
	f.mu.Unlock()
	close(done)
	return err
}

// rememberPointer records a floating-ref resolution (branch/semver/HEAD →
// immutable resolved form) with the current timestamp, unless the fresh
// value came from that very pointer. No-ref (HEAD) resolutions are cached
// under their own empty-ref key, same as every other floating ref.
func (f *Fetcher) rememberPointer(url, refSpec, resolved string, fresh bool) {
	if f == nil || f.dir == "" || refSpec == resolved || fresh {
		return
	}
	m := meta{URL: url, Ref: refSpec, Resolved: resolved, FetchedAt: time.Now().Format(time.RFC3339)}
	writePointer(f.dir, cacheKey(url, refSpec), m)
}

// fresh reports whether a floating-ref resolution is still within the TTL.
// With ttl == 0 nothing is fresh — floating refs always re-resolve (the
// fresh resolution is still written back for later runs).
func (f *Fetcher) fresh(m meta) bool {
	if f == nil || f.ttl <= 0 {
		return false
	}
	ts, err := time.Parse(time.RFC3339, m.FetchedAt)
	if err != nil {
		return false
	}
	return time.Since(ts) < f.ttl
}

// resolveFloating resolves a floating ref to its immutable form without a
// clone: branch/HEAD via one ls-remote (resolved to the commit sha), semver
// via the tag list (resolved to the winning tag). The second return value
// is the remote ref name the clone should fetch shallowly (empty when the
// resolved form is enough — tags, or HEAD over a server that advertises a
// real HEAD hash, where the default clone already lands on it).
func resolveFloating(ctx context.Context, url string, ref *flux.GitRepositoryRef) (resolved, cloneRef string, err error) {
	switch {
	case ref != nil && ref.Branch != "":
		sha, err := lsRemoteHash(ctx, url, "refs/heads/"+ref.Branch)
		if err != nil {
			return "", "", fmt.Errorf("resolving branch %q of %s: %w", ref.Branch, url, err)
		}
		return "commit:" + sha, "refs/heads/" + ref.Branch, nil
	case ref != nil && ref.Semver != "":
		refs, err := listRefs(ctx, url)
		if err != nil {
			return "", "", fmt.Errorf("listing tags of %s for semver %q: %w", url, ref.Semver, err)
		}
		tag, err := pickSemverTag(refs, ref.Semver)
		if err != nil {
			return "", "", fmt.Errorf("resolving semver %q of %s: %w", ref.Semver, url, err)
		}
		return "tag:" + tag, "", nil
	default:
		sha, branch, err := lsRemoteHead(ctx, url)
		if err != nil {
			return "", "", fmt.Errorf("resolving HEAD of %s: %w", url, err)
		}
		if branch == "" {
			return "commit:" + sha, "", nil
		}
		// Zero-hash HEAD worked around via a named branch — clone that
		// branch shallowly instead of defaulting to a full clone.
		return "commit:" + sha, "refs/heads/" + branch, nil
	}
}

// cloneAtomic clones url at resolved into dir via a temp sibling and a
// rename, so a reader never sees a partial directory: dir either does not
// exist or holds a complete clone with a valid seed. A rename race with a
// concurrent process is fine — the loser discards its temp and reuses the
// winner's directory.
func cloneAtomic(ctx context.Context, cacheDir, url, resolved, cloneRef, dir string) error {
	if err := os.MkdirAll(filepath.Join(cacheDir, "tmp"), 0o755); err != nil {
		return fmt.Errorf("preparing git source cache: %w", err)
	}
	// data/ must exist before the publish rename below — rename does not
	// create missing parents.
	if err := os.MkdirAll(filepath.Join(cacheDir, "data"), 0o755); err != nil {
		return fmt.Errorf("preparing git source cache: %w", err)
	}
	tmp, err := os.MkdirTemp(filepath.Join(cacheDir, "tmp"), "clone-*")
	if err != nil {
		return fmt.Errorf("preparing git source clone: %w", err)
	}
	if _, err := cloneOnce(ctx, url, resolved, cloneRef, tmp); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	m := meta{URL: url, Ref: resolved, Resolved: resolved, FetchedAt: time.Now().Format(time.RFC3339)}
	seed, err := json.MarshalIndent(m, "", "  ")
	if err == nil {
		err = os.WriteFile(filepath.Join(tmp, seedFileName), seed, 0o644)
	}
	if err != nil {
		os.RemoveAll(tmp)
		return fmt.Errorf("recording git source seed: %w", err)
	}

	if err := os.Rename(tmp, dir); err != nil {
		// Another process won the race; reuse its directory if complete.
		if seedValid(dir) {
			os.RemoveAll(tmp)
			return nil
		}
		// The target exists but is broken (interrupted copy, corrupted
		// seed) — replace it while holding nothing else.
		os.RemoveAll(dir)
		if err := os.Rename(tmp, dir); err != nil {
			os.RemoveAll(tmp)
			return fmt.Errorf("publishing git source clone: %w", err)
		}
	}
	return nil
}

// cloneOnce clones url at resolved into dst (which must exist) and returns
// dst. cloneRef, when set, names the remote ref to fetch instead of the
// resolved commit — used for branch/HEAD resolutions so the tip clone can
// be shallow (depth 1): the resolved sha IS the tip, the tree is identical,
// the content-addressed key is unchanged.
//
// Shallow clones (depth 1) are used for the protocols whose servers
// guarantee them (http/https/git); file and other transports do a full
// clone — local fixtures and exotic servers are small or rare enough that
// correctness beats the bandwidth. A commit checkout without a cloneRef
// (ref.commit from a manifest) always needs the full history: shallow
// fetch of an arbitrary SHA is not guaranteed by any server.
func cloneOnce(ctx context.Context, url, resolved, cloneRef, dst string) (string, error) {
	kind, value, _ := strings.Cut(resolved, ":")

	opts := &gogit.CloneOptions{URL: url}
	shallow := supportsShallow(url) && (kind != "commit" || cloneRef != "")
	switch {
	case cloneRef != "":
		opts.ReferenceName = plumbing.ReferenceName(cloneRef)
	case kind == "tag":
		opts.ReferenceName = plumbing.NewTagReferenceName(value)
	case kind == "commit":
		// Full clone; checkout below.
	default:
		return "", fmt.Errorf("internal error: unresolved ref spec %q", resolved)
	}
	if shallow {
		opts.Depth = 1
		opts.SingleBranch = true
	}

	_, err := gogit.PlainCloneContext(ctx, dst, false, opts)
	if err != nil && shallow {
		// A server may refuse the shallow form of this ref; retry full.
		os.RemoveAll(dst)
		opts.Depth = 0
		opts.SingleBranch = false
		_, err = gogit.PlainCloneContext(ctx, dst, false, opts)
	}
	if err != nil {
		os.RemoveAll(dst)
		return "", fmt.Errorf("cloning %s at %s: %w", url, resolved, err)
	}

	if kind == "commit" && cloneRef == "" {
		repo, err := gogit.PlainOpen(dst)
		if err != nil {
			os.RemoveAll(dst)
			return "", fmt.Errorf("opening clone of %s: %w", url, err)
		}
		w, err := repo.Worktree()
		if err != nil {
			os.RemoveAll(dst)
			return "", fmt.Errorf("opening worktree of %s clone: %w", url, err)
		}
		if err := w.Checkout(&gogit.CheckoutOptions{Hash: plumbing.NewHash(value)}); err != nil {
			os.RemoveAll(dst)
			return "", fmt.Errorf("checking out %s of %s: %w", value, url, err)
		}
	}
	return dst, nil
}

// supportsShallow reports whether the URL scheme goes through transports
// whose servers generally honor depth-limited ref fetches. The file
// transport (local fixtures, tests) is excluded on purpose: go-git's
// in-process server has historically been uneven here, and local clones
// cost nothing.
func supportsShallow(url string) bool {
	return strings.HasPrefix(url, "http://") ||
		strings.HasPrefix(url, "https://") ||
		strings.HasPrefix(url, "git://")
}

// listRefs runs one ls-remote against url over an in-memory repository —
// no local clone or state needed.
func listRefs(ctx context.Context, url string) ([]*plumbing.Reference, error) {
	repo, err := gogit.Init(memory.NewStorage(), nil)
	if err != nil {
		return nil, err
	}
	remote, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{url}})
	if err != nil {
		return nil, err
	}
	return remote.ListContext(ctx, &gogit.ListOptions{})
}

// lsRemoteHash resolves one exact ref name to its commit sha.
func lsRemoteHash(ctx context.Context, url, refName string) (string, error) {
	refs, err := listRefs(ctx, url)
	if err != nil {
		return "", err
	}
	for _, ref := range refs {
		if ref.Name() == plumbing.ReferenceName(refName) && !ref.Hash().IsZero() {
			return ref.Hash().String(), nil
		}
	}
	return "", fmt.Errorf("remote has no %s ref", refName)
}

// lsRemoteHead resolves the remote's default branch tip, returning its
// commit sha and, when the resolution came from a named branch rather than
// a real HEAD advertisement, that branch's name (empty otherwise). Some
// transports (go-git's in-process file server) advertise HEAD as a
// zero-hash symbolic reference: a usable HEAD hash is preferred, the single
// existing branch is the fallback, then the conventional default branch
// names (main, master); several branches without any of those are an
// explicit error rather than a guess.
func lsRemoteHead(ctx context.Context, url string) (sha, branch string, err error) {
	refs, err := listRefs(ctx, url)
	if err != nil {
		return "", "", err
	}
	branches := map[string]string{} // name → sha
	for _, ref := range refs {
		name := ref.Name().String()
		switch {
		case name == "HEAD":
			if !ref.Hash().IsZero() {
				return ref.Hash().String(), "", nil
			}
		case strings.HasPrefix(name, "refs/heads/"):
			branches[strings.TrimPrefix(name, "refs/heads/")] = ref.Hash().String()
		}
	}
	if len(branches) == 0 {
		return "", "", fmt.Errorf("remote advertises no branches")
	}
	if len(branches) == 1 {
		for name, hash := range branches {
			return hash, name, nil
		}
	}
	for _, name := range []string{"main", "master"} {
		if hash, ok := branches[name]; ok {
			return hash, name, nil
		}
	}
	return "", "", fmt.Errorf("cannot determine default branch (zero-hash HEAD, %d branches)", len(branches))
}

// cacheKey is the shared content key of a (normalized url, ref spec) pair.
func cacheKey(url, refSpec string) string {
	h := sha256.Sum256([]byte(git.NormalizeGitURL(url) + "\x00" + refSpec))
	return hex.EncodeToString(h[:])
}

// seedValid reports whether dir holds a complete clone: the seed file
// exists, parses and names a resolution.
func seedValid(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, seedFileName))
	if err != nil {
		return false
	}
	var m meta
	if err := json.Unmarshal(data, &m); err != nil {
		return false
	}
	return m.Resolved != ""
}

// pointerPath is the floating-ref pointer file for key.
func pointerPath(cacheDir, key string) string {
	return filepath.Join(cacheDir, "refs", key+".json")
}

// readPointer loads the floating-ref pointer for key, if present and valid.
func readPointer(cacheDir, key string) (meta, bool) {
	data, err := os.ReadFile(pointerPath(cacheDir, key))
	if err != nil {
		return meta{}, false
	}
	var m meta
	if err := json.Unmarshal(data, &m); err != nil {
		return meta{}, false
	}
	if m.Resolved == "" {
		return meta{}, false
	}
	return m, true
}

// writePointer atomically stores the floating-ref pointer for key (temp
// file in the same directory, then rename).
func writePointer(cacheDir, key string, m meta) {
	p := pointerPath(cacheDir, key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "ptr-*")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return
	}
	// Rename over an existing pointer is atomic on POSIX: the new pointer
	// replaces the old one in one step.
	_ = os.Rename(name, p)
}
