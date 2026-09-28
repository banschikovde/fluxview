// Package cachedir defines the cache directory conventions shared by every
// fluxview cache: exactly one disable word, "disabled" (case-insensitive) —
// a cache directory flag or env var set to it turns that cache off for the
// run. The spell, the retired-word migration and the shared cache base all
// live here so a new cache cannot silently invent its own spelling.
package cachedir

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Disabled reports whether a cache directory value is the disable word
// "disabled" (case-insensitive). A padded value is NOT the word: padding is
// rejected uniformly on every path (flag, per-cache env, base env), so the
// same sloppy input can never disable silently on one path and fall back
// to the default on another.
func Disabled(dir string) bool {
	return strings.EqualFold(dir, "disabled")
}

// retired reports whether a value is one of the pre-unification disable
// words. They no longer disable — flags reject them with a hint, env vars
// warn once and migrate — so they can never silently become directory
// names again.
func retired(dir string) bool {
	v := strings.TrimSpace(dir)
	return strings.EqualFold(v, "off") || strings.EqualFold(v, "none")
}

// Flag adapts a *string cache directory flag target to pflag.Value: the
// disable word normalizes to its canonical "disabled" spelling, the retired
// words fail with a hint to the canonical one, and clearly erroneous values
// — empty or padded with whitespace — are rejected instead of silently
// becoming directory names. It implements pflag.Value without importing
// pflag.
type Flag struct{ p *string }

// NewFlag wraps a cache directory flag target, pre-seeded with its default.
func NewFlag(p *string) *Flag { return &Flag{p: p} }

// String returns the current value (the flag default before Set).
func (f *Flag) String() string {
	if f == nil || f.p == nil {
		return ""
	}
	return *f.p
}

// Set validates and stores a value from the command line.
func (f *Flag) Set(v string) error {
	switch {
	case v == "":
		return errors.New("cache directory cannot be empty; omit the flag to use the default")
	case v != strings.TrimSpace(v):
		return fmt.Errorf("cache directory %q has leading or trailing whitespace", v)
	case retired(v):
		return fmt.Errorf("cache directory %q is a retired disable word, use \"disabled\"", v)
	}
	if Disabled(v) {
		*f.p = "disabled"
		return nil
	}
	*f.p = v
	return nil
}

// Type names the value kind in flag usage and error messages.
func (f *Flag) Type() string { return "cache-dir" }

// CacheHomeEnv names the base directory env var of every fluxview cache.
// Besides a path it accepts the disable word: FLUXVIEW_CACHE_HOME=disabled
// turns every cache off for the run (the global switch) — per-cache env
// vars still override it for their cache.
const CacheHomeEnv = "FLUXVIEW_CACHE_HOME"

// DefaultDir resolves a cache's directory in one place, the same for every
// cache: the cache's own env var wins (disable word included), then the
// global switch (FLUXVIEW_CACHE_HOME=disabled disables every cache that did
// not get its own value), else the cache lives under Home() as <name>.
// The base accepts no retired words — off/none never applied to it — and a
// padded base value warns once and means "no base override", mirroring the
// per-cache env policy exactly.
func DefaultDir(envName, name string) string {
	if dir := EnvDir(envName); dir != "" {
		return dir
	}
	base := os.Getenv(CacheHomeEnv)
	if v := strings.TrimSpace(base); v != "" {
		switch {
		case v != base:
			warnEnvOnce(CacheHomeEnv, "Warning: invalid %s %q (leading or trailing whitespace), using per-cache defaults\n", CacheHomeEnv, base)
		case Disabled(v):
			return "disabled" // the global switch: every cache off
		case retired(v):
			warnEnvOnce(CacheHomeEnv, "Warning: invalid %s %q (retired disable words apply to the per-cache vars; the base takes a path or \"disabled\"), using the default cache base\n", CacheHomeEnv, base)
		}
	}
	return filepath.Join(Home(), name)
}

// EnvDir reads a cache directory environment variable with the same policy
// the -dir flags enforce: unset or empty means "use the default" (the caller
// falls through), the disable word normalizes to the canonical "disabled",
// a retired word warns once and migrates to "disabled", and a value padded
// with whitespace is unusable — warn once and treat it as unset. Env values
// cannot fail a flag parse, so they warn and fall back instead of erroring,
// the same convention the TTL env vars follow.
func EnvDir(name string) string {
	v := os.Getenv(name)
	if v == "" {
		return ""
	}
	if v != strings.TrimSpace(v) {
		warnEnvOnce(name, "Warning: invalid %s %q (leading or trailing whitespace), using the default cache directory\n", name, v)
		return ""
	}
	if retired(v) {
		warnEnvOnce(name, "Warning: retired disable word %q in %s, treating as \"disabled\"\n", v, name)
		return "disabled"
	}
	if Disabled(v) {
		return "disabled"
	}
	return v
}

// Home returns the base directory every fluxview cache lives under:
// $FLUXVIEW_CACHE_HOME, else ~/.cache/fluxview. One root, one env var;
// DefaultDir composes the per-cache defaults from it. The base itself
// cannot be disabled — the disable word on it is the global switch handled
// by DefaultDir; a padded value warns once and falls back to the default.
func Home() string {
	if v := os.Getenv(CacheHomeEnv); v != "" {
		if v != strings.TrimSpace(v) {
			warnEnvOnce(CacheHomeEnv, "Warning: invalid %s %q (leading or trailing whitespace), using the default cache base\n", CacheHomeEnv, v)
		} else if !Disabled(v) && !retired(v) {
			return v
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "fluxview-cache")
	}
	return filepath.Join(home, ".cache", "fluxview")
}

// warnOut is where env warnings go: os.Stderr in production, swapped in
// tests.
var warnOut io.Writer = os.Stderr

var (
	warnMu   sync.Mutex
	warnOnce = make(map[string]*sync.Once)
)

// warnEnvOnce keeps each variable's warning to a single line per process.
func warnEnvOnce(name, format string, args ...any) {
	warnMu.Lock()
	once, ok := warnOnce[name]
	if !ok {
		once = new(sync.Once)
		warnOnce[name] = once
	}
	warnMu.Unlock()
	once.Do(func() { fmt.Fprintf(warnOut, format, args...) })
}
