// Package sourcecache holds the on-disk cache plumbing shared by the
// external source fetchers (internal/gitsource, internal/ocisource):
// content-addressed directories published atomically with a seed file
// marking completeness, floating-ref pointer files with a TTL, and
// per-process in-flight deduplication. All of it is deliberately boring —
// the fetchers own the network and the extraction; this package owns the
// "never expose a partial fetch" and "never race a concurrent fetch of
// the same key" guarantees.
package sourcecache

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
)

// Meta is the shared shape of the in-directory seed file and the
// floating-ref pointer: what was requested, what it resolved to, and when.
type Meta struct {
	URL       string `json:"url"`
	Ref       string `json:"ref"`
	Resolved  string `json:"resolved"`
	FetchedAt string `json:"fetchedAt"`
}

// Key is the shared content key of a source: sha256 over the parts
// (the fetcher passes its identity parts — normalized URL, resolved ref,
// …) joined with NUL bytes.
func Key(parts ...string) string {
	h := sha256.Sum256([]byte(joinNul(parts)))
	return hex.EncodeToString(h[:])
}

func joinNul(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "\x00"
		}
		out += p
	}
	return out
}

// WriteSeed records the provenance of one fetched directory inside the
// directory itself; its presence and integrity mark the directory
// complete (WriteSeed is the last step before publishing).
func WriteSeed(dir, fileName string, m Meta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding source seed: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileName), data, 0o644); err != nil {
		return fmt.Errorf("writing source seed: %w", err)
	}
	return nil
}

// SeedValid reports whether dir holds a complete fetch: the seed file
// exists, parses and names a resolution.
func SeedValid(dir, fileName string) bool {
	data, err := os.ReadFile(filepath.Join(dir, fileName))
	if err != nil {
		return false
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return false
	}
	return m.Resolved != ""
}

// Fresh reports whether a floating-ref resolution is still within the
// TTL. With ttl == 0 nothing is fresh — floating refs always re-resolve
// (the fresh resolution is still written back for later runs).
func Fresh(ttl time.Duration, m Meta) bool {
	if ttl <= 0 {
		return false
	}
	ts, err := time.Parse(time.RFC3339, m.FetchedAt)
	if err != nil {
		return false
	}
	return time.Since(ts) < ttl
}

// ReadPointer loads the floating-ref pointer for key, if present and
// valid. Pointers live under <cacheDir>/refs/<key>.json.
func ReadPointer(cacheDir, key string) (Meta, bool) {
	data, err := os.ReadFile(pointerPath(cacheDir, key))
	if err != nil {
		return Meta{}, false
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return Meta{}, false
	}
	if m.Resolved == "" {
		return Meta{}, false
	}
	return m, true
}

// WritePointer atomically stores the floating-ref pointer for key (temp
// file in the same directory, then rename). Failures are silent: a stale
// or missing pointer only costs one re-resolution.
func WritePointer(cacheDir, key string, m Meta) {
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

func pointerPath(cacheDir, key string) string {
	return filepath.Join(cacheDir, "refs", key+".json")
}

// Dedup serializes work per key within one process: the second caller
// waits for the first one's function instead of racing it with a
// duplicate.
type Dedup struct {
	mu       sync.Mutex
	inflight map[string]chan struct{}
}

// NewDedup returns an empty dedup tracker.
func NewDedup() *Dedup {
	return &Dedup{inflight: make(map[string]chan struct{})}
}

// Do runs fn for key, or waits for the in-flight fn of the same key and
// then reports done() — whether the winner's work left a usable result
// (typically a SeedValid check the caller supplies). When the winner
// failed, the caller retries by invoking Do again.
func (d *Dedup) Do(ctx context.Context, key string, done func() bool, fn func() error) error {
	d.mu.Lock()
	if wait, ok := d.inflight[key]; ok {
		d.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
		if done() {
			return nil
		}
		// The first attempt failed; try again ourselves.
		return d.Do(ctx, key, done, fn)
	}
	finished := make(chan struct{})
	d.inflight[key] = finished
	d.mu.Unlock()

	err := fn()

	d.mu.Lock()
	delete(d.inflight, key)
	d.mu.Unlock()
	close(finished)
	return err
}

// Publish atomically moves tmp to dir, tolerating a concurrent winner:
// if dir already holds a complete result (valid), the loser's tmp is
// dropped and the winner's directory is as good as ours. tmp is consumed
// or removed — it never lingers on return.
func Publish(tmp, dir string, valid func(dir string) bool) error {
	// The parent must exist before the rename — rename does not create
	// missing parents.
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("preparing source cache: %w", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		if valid(dir) {
			os.RemoveAll(tmp)
			return nil
		}
		// The target exists but is broken (interrupted fetch, corrupted
		// seed) — replace it while holding nothing else.
		os.RemoveAll(dir)
		if err := os.Rename(tmp, dir); err != nil {
			os.RemoveAll(tmp)
			return fmt.Errorf("publishing source to cache: %w", err)
		}
	}
	return nil
}

// ttlWarnOnce keeps the invalid-env warning to a single line per env per
// process (the same convention as the Helm cache).
var (
	ttlWarnMu   sync.Mutex
	ttlWarnOnce = map[string]bool{}
)

// TTLFromEnv reads a source-cache TTL env (Go duration) with the shared
// convention: unset → 10 minutes; invalid → one warning line naming the
// env, then the default; zero is returned as-is (always re-resolve).
func TTLFromEnv(envName string) time.Duration {
	const def = 10 * time.Minute
	v := os.Getenv(envName)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		ttlWarnMu.Lock()
		warned := ttlWarnOnce[envName]
		ttlWarnOnce[envName] = true
		ttlWarnMu.Unlock()
		if !warned {
			fmt.Fprintf(os.Stderr, "Warning: invalid %s %q, using %s\n", envName, v, def)
		}
		return def
	}
	return d
}

// NoFetchFromEnv reads a source-fetch kill-switch env: 1/true/yes/on
// (case-insensitive, whitespace-tolerant) means off.
func NoFetchFromEnv(envName string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envName))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// TempBase is the per-process base directory for cache-disabled fetches:
// created lazily on first use, removed by Remove, never reused across
// runs.
type TempBase struct {
	mu  sync.Mutex
	dir string
}

// Dir creates the base directory on first use (MkdirTemp with prefix) and
// returns it.
func (t *TempBase) Dir(prefix string) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dir == "" {
		dir, err := os.MkdirTemp("", prefix)
		if err != nil {
			return "", fmt.Errorf("preparing source fetch: %w", err)
		}
		t.dir = dir
	}
	return t.dir, nil
}

// Remove deletes the base directory, if one was created. Safe when unused.
func (t *TempBase) Remove() error {
	t.mu.Lock()
	dir := t.dir
	t.mu.Unlock()
	if dir == "" {
		return nil
	}
	return os.RemoveAll(dir)
}

// RememberPointer records a floating-ref resolution (→ immutable resolved
// form) with the current timestamp under the caller's key (each fetcher
// keys by its own URL normalization), unless the value came from that very
// pointer (fresh) or the ref is its own resolved form. cacheDir empty is a
// no-op (disabled cache).
func RememberPointer(cacheDir, key, url, refSpec, resolved string, fresh bool) {
	if cacheDir == "" || refSpec == resolved || fresh {
		return
	}
	WritePointer(cacheDir, key, Meta{
		URL: url, Ref: refSpec, Resolved: resolved, FetchedAt: time.Now().Format(time.RFC3339),
	})
}

// ScratchDir allocates a fresh temp directory under <cacheDir>/tmp for an
// in-progress fetch; the caller removes it on failure or publishes it via
// Publish.
func ScratchDir(cacheDir, pattern string) (string, error) {
	tmp := filepath.Join(cacheDir, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", fmt.Errorf("preparing source cache: %w", err)
	}
	dir, err := os.MkdirTemp(tmp, pattern)
	if err != nil {
		return "", fmt.Errorf("preparing source fetch: %w", err)
	}
	return dir, nil
}
