package cachedir

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestDisabled(t *testing.T) {
	for _, tc := range []struct {
		dir  string
		want bool
	}{
		{"disabled", true},
		{"DISABLED", true},
		{"Disabled", true},
		{" disabled ", false}, // padded is not the word — padding is handled uniformly on every path
		{"", false},
		{"/some/path", false},
		{"off", false}, // retired, no longer a disable word
		{"none", false},
		{"disable", false},
	} {
		if got := Disabled(tc.dir); got != tc.want {
			t.Errorf("Disabled(%q) = %v, want %v", tc.dir, got, tc.want)
		}
	}
}

func TestFlagSet(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"disable word", "disabled", "disabled", false},
		{"upper case canonicalized", "DISABLED", "disabled", false},
		{"retired off rejected", "off", "", true},
		{"retired none rejected", "none", "", true},
		{"retired OFF rejected", "OFF", "", true},
		{"path passes through", "/tmp/cache", "/tmp/cache", false},
		{"relative path", ".cache/helm", ".cache/helm", false},
		{"empty rejected", "", "", true},
		{"leading space rejected", " /tmp", "", true},
		{"trailing space rejected", "/tmp ", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cur := "default"
			f := NewFlag(&cur)
			err := f.Set(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Set(%q) = nil error, want one", tc.in)
				}
				if cur != "default" {
					t.Errorf("rejected value must not overwrite: cur = %q", cur)
				}
				return
			}
			if err != nil {
				t.Fatalf("Set(%q): %v", tc.in, err)
			}
			if cur != tc.want {
				t.Errorf("Set(%q) stored %q, want %q", tc.in, cur, tc.want)
			}
		})
	}
}

func TestFlagStringAndType(t *testing.T) {
	cur := "/some/default"
	f := NewFlag(&cur)
	if got := f.String(); got != "/some/default" {
		t.Errorf("String() = %q, want the pre-seeded default", got)
	}
	if got := f.Type(); got != "cache-dir" {
		t.Errorf("Type() = %q, want cache-dir", got)
	}
	var nilFlag *Flag
	if got := nilFlag.String(); got != "" {
		t.Errorf("nil Flag String() = %q, want empty", got)
	}
}

func TestEnvDir(t *testing.T) {
	// Capture warnings; unique variable names keep the per-name warn-once
	// state independent across subtests.
	var buf syncBuffer
	warnOut = &buf
	t.Cleanup(func() { warnOut = os.Stderr })

	for _, tc := range []struct {
		name string
		run  func(*testing.T, *syncBuffer)
	}{
		{"unset reads as empty", envDirUnsetReadsAsEmpty},
		{"plain value passes through", envDirPlainValuePassesThrough},
		{"inner spaces are a legal path", envDirInnerSpacesAreALegalPath},
		{"disable word canonicalizes", envDirDisableWordCanonicalizes},
		{"retired word warns once and migrates to disabled", envDirRetiredWordWarnsOnceAndMigratesToDisabled},
		{"padded value warns once and reads as unset", envDirPaddedValueWarnsOnceAndReadsAsUnset},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, &buf) })
	}
}

// unset reads as empty
func envDirUnsetReadsAsEmpty(t *testing.T, buf *syncBuffer) {
	t.Setenv("FLUXVIEW_TEST_ENVDIR_UNSET", "")
	if got := EnvDir("FLUXVIEW_TEST_ENVDIR_UNSET"); got != "" {
		t.Errorf("EnvDir = %q, want empty", got)
	}
}

// plain value passes through
func envDirPlainValuePassesThrough(t *testing.T, buf *syncBuffer) {
	t.Setenv("FLUXVIEW_TEST_ENVDIR_PLAIN", "/some/cache")
	if got := EnvDir("FLUXVIEW_TEST_ENVDIR_PLAIN"); got != "/some/cache" {
		t.Errorf("EnvDir = %q, want /some/cache", got)
	}
}

// inner spaces are a legal path
func envDirInnerSpacesAreALegalPath(t *testing.T, buf *syncBuffer) {
	t.Setenv("FLUXVIEW_TEST_ENVDIR_SPACES", "/tmp/my cache")
	if got := EnvDir("FLUXVIEW_TEST_ENVDIR_SPACES"); got != "/tmp/my cache" {
		t.Errorf("EnvDir = %q, want the value unchanged", got)
	}
}

// disable word canonicalizes
func envDirDisableWordCanonicalizes(t *testing.T, buf *syncBuffer) {
	t.Setenv("FLUXVIEW_TEST_ENVDIR_SPELL", "DISABLED")
	if got := EnvDir("FLUXVIEW_TEST_ENVDIR_SPELL"); got != "disabled" {
		t.Errorf("EnvDir = %q, want disabled", got)
	}
}

// retired word warns once and migrates to disabled
func envDirRetiredWordWarnsOnceAndMigratesToDisabled(t *testing.T, buf *syncBuffer) {
	t.Setenv("FLUXVIEW_TEST_ENVDIR_RETIRED", "off")
	for range 2 {
		if got := EnvDir("FLUXVIEW_TEST_ENVDIR_RETIRED"); got != "disabled" {
			t.Fatalf("EnvDir = %q, want disabled (retired word migrates)", got)
		}
	}
	if got := strings.Count(buf.String(), "FLUXVIEW_TEST_ENVDIR_RETIRED"); got != 1 {
		t.Errorf("warning printed %d times, want exactly 1:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), `retired disable word "off"`) {
		t.Errorf("warning should name the retired word, got: %s", buf.String())
	}
}

// padded value warns once and reads as unset
func envDirPaddedValueWarnsOnceAndReadsAsUnset(t *testing.T, buf *syncBuffer) {
	t.Setenv("FLUXVIEW_TEST_ENVDIR_PADDED", " /tmp ")
	for range 2 {
		if got := EnvDir("FLUXVIEW_TEST_ENVDIR_PADDED"); got != "" {
			t.Fatalf("EnvDir = %q, want empty (value unusable)", got)
		}
	}
	if got := strings.Count(buf.String(), "FLUXVIEW_TEST_ENVDIR_PADDED"); got != 1 {
		t.Errorf("warning printed %d times, want exactly 1:\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), "leading or trailing whitespace") {
		t.Errorf("warning should name the reason, got: %s", buf.String())
	}
}

func TestDefaultDir(t *testing.T) {
	// Capture warnings; unique variable names keep the per-name warn-once
	// state independent across subtests.
	var buf syncBuffer
	warnOut = &buf
	t.Cleanup(func() { warnOut = os.Stderr })

	for _, tc := range []struct {
		name string
		run  func(*testing.T, *syncBuffer)
	}{
		{"own env wins", defaultDirOwnEnvWins},
		{"global switch disables every cache without its own value", defaultDirGlobalSwitchDisablesEveryCacheWithoutItsOwnValue},
		{"own path overrides the global switch for its cache", defaultDirOwnPathOverridesTheGlobalSwitchForItsCache},
		{"own disabled word survives the global path", defaultDirOwnDisabledWordSurvivesTheGlobalPath},
		{"retired word on the base is invalid, not the switch", defaultDirRetiredWordOnTheBaseIsInvalidNotTheSwitch},
		{"padded disable word on the base does not silently disable all", defaultDirPaddedDisableWordOnTheBaseDoesNotSilentlyDisableAll},
		{"default is Home()/name", defaultDirDefaultIsHomeName},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, &buf) })
	}
}

// own env wins
func defaultDirOwnEnvWins(t *testing.T, buf *syncBuffer) {
	t.Setenv("FLUXVIEW_TEST_DD_DIR", "/own/dir")
	t.Setenv(CacheHomeEnv, "/base")
	if got := DefaultDir("FLUXVIEW_TEST_DD_DIR", "helm"); got != "/own/dir" {
		t.Errorf("DefaultDir = %q, want /own/dir", got)
	}
}

// global switch disables every cache without its own value
func defaultDirGlobalSwitchDisablesEveryCacheWithoutItsOwnValue(t *testing.T, buf *syncBuffer) {
	t.Setenv("FLUXVIEW_TEST_DD_DIR", "")
	t.Setenv(CacheHomeEnv, "disabled")
	if got := DefaultDir("FLUXVIEW_TEST_DD_DIR", "helm"); got != "disabled" {
		t.Errorf("DefaultDir = %q, want disabled (global switch)", got)
	}
}

// own path overrides the global switch for its cache
func defaultDirOwnPathOverridesTheGlobalSwitchForItsCache(t *testing.T, buf *syncBuffer) {
	t.Setenv("FLUXVIEW_TEST_DD_DIR", "/own/dir")
	t.Setenv(CacheHomeEnv, "disabled")
	if got := DefaultDir("FLUXVIEW_TEST_DD_DIR", "helm"); got != "/own/dir" {
		t.Errorf("DefaultDir = %q, want /own/dir (specific beats general)", got)
	}
}

// own disabled word survives the global path
func defaultDirOwnDisabledWordSurvivesTheGlobalPath(t *testing.T, buf *syncBuffer) {
	t.Setenv("FLUXVIEW_TEST_DD_DIR", "disabled")
	t.Setenv(CacheHomeEnv, "/base")
	if got := DefaultDir("FLUXVIEW_TEST_DD_DIR", "helm"); got != "disabled" {
		t.Errorf("DefaultDir = %q, want disabled", got)
	}
}

// retired word on the base is invalid, not the switch
func defaultDirRetiredWordOnTheBaseIsInvalidNotTheSwitch(t *testing.T, buf *syncBuffer) {
	t.Setenv("FLUXVIEW_TEST_DD_DIR", "")
	t.Setenv(CacheHomeEnv, "off")
	if got := DefaultDir("FLUXVIEW_TEST_DD_DIR", "helm"); got != filepath.Join(homeDir(t), ".cache", "fluxview", "helm") {
		t.Errorf("DefaultDir = %q, want the default base (the base never had disable words)", got)
	}
	if !strings.Contains(buf.String(), "retired disable words") {
		t.Errorf("expected an invalid-value warning, got: %s", buf.String())
	}
}

// padded disable word on the base does not silently disable all
func defaultDirPaddedDisableWordOnTheBaseDoesNotSilentlyDisableAll(t *testing.T, buf *syncBuffer) {
	resetWarnOnce()
	t.Setenv("FLUXVIEW_TEST_DD_DIR", "")
	t.Setenv(CacheHomeEnv, " disabled ")
	if got := DefaultDir("FLUXVIEW_TEST_DD_DIR", "helm"); got != filepath.Join(homeDir(t), ".cache", "fluxview", "helm") {
		t.Errorf("DefaultDir = %q, want the default base (padded value is unusable, same as the per-cache env policy)", got)
	}
	if !strings.Contains(buf.String(), "leading or trailing whitespace") {
		t.Errorf("expected a padding warning, got: %s", buf.String())
	}
}

// default is Home()/name
func defaultDirDefaultIsHomeName(t *testing.T, buf *syncBuffer) {
	t.Setenv("FLUXVIEW_TEST_DD_DIR", "")
	t.Setenv(CacheHomeEnv, "/base")
	if got := DefaultDir("FLUXVIEW_TEST_DD_DIR", "helm"); got != "/base/helm" {
		t.Errorf("DefaultDir = %q, want /base/helm", got)
	}
}

func TestHome(t *testing.T) {
	// Capture warnings; unique variable names keep the per-name warn-once
	// state independent across subtests.
	var buf syncBuffer
	warnOut = &buf
	resetWarnOnce()
	t.Cleanup(func() { warnOut = os.Stderr })

	for _, tc := range []struct {
		name string
		run  func(*testing.T, *syncBuffer)
	}{
		{"default is ~/.cache/fluxview", homeDefaultIsCacheFluxview},
		{"env relocates the base", homeEnvRelocatesTheBase},
		{"padded value warns once and falls back", homePaddedValueWarnsOnceAndFallsBack},
		{"disable word is not a path here (DefaultDir owns the switch)", homeDisableWordIsNotAPathHereDefaultDirOwnsTheSwitch},
		{"no home directory falls back to a private unpredictable temp base", homeNoHomeDirectoryFallsBackToAPrivateUnpredictableTempBase},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, &buf) })
	}
}

// default is ~/.cache/fluxview
func homeDefaultIsCacheFluxview(t *testing.T, buf *syncBuffer) {
	t.Setenv(CacheHomeEnv, "")
	if got := Home(); got != filepath.Join(homeDir(t), ".cache", "fluxview") {
		t.Errorf("Home() = %q, want ~/.cache/fluxview", got)
	}
}

// env relocates the base
func homeEnvRelocatesTheBase(t *testing.T, buf *syncBuffer) {
	t.Setenv(CacheHomeEnv, "/cache/root")
	if got := Home(); got != "/cache/root" {
		t.Errorf("Home() = %q, want /cache/root", got)
	}
}

// padded value warns once and falls back
func homePaddedValueWarnsOnceAndFallsBack(t *testing.T, buf *syncBuffer) {
	t.Setenv(CacheHomeEnv, " /padded ")
	if got := Home(); got != filepath.Join(homeDir(t), ".cache", "fluxview") {
		t.Errorf("Home() = %q, want the default", got)
	}
	if got := strings.Count(buf.String(), CacheHomeEnv); got != 1 {
		t.Errorf("expected exactly one warning, got %d mentions:\n%s", got, buf.String())
	}
	t.Setenv(CacheHomeEnv, " /padded ")
	_ = Home()
	if got := strings.Count(buf.String(), CacheHomeEnv); got != 1 {
		t.Errorf("warn-once violated: %d mentions after repeats\n%s", got, buf.String())
	}
}

// disable word is not a path here (DefaultDir owns the switch)
func homeDisableWordIsNotAPathHereDefaultDirOwnsTheSwitch(t *testing.T, buf *syncBuffer) {
	t.Setenv(CacheHomeEnv, "disabled")
	if got := Home(); got != filepath.Join(homeDir(t), ".cache", "fluxview") {
		t.Errorf("Home() = %q, want the default (the switch is handled by DefaultDir)", got)
	}
}

// no home directory falls back to a private unpredictable temp base
func homeNoHomeDirectoryFallsBackToAPrivateUnpredictableTempBase(t *testing.T, buf *syncBuffer) {
	if runtime.GOOS == "windows" {
		t.Skip("os.UserHomeDir reads USERPROFILE and os.TempDir ignores TMPDIR on windows; the fallback path needs a windows-specific harness")
	}
	tmp := t.TempDir() // hermetic TMPDIR so the assertions never see the real /tmp
	t.Setenv("TMPDIR", tmp)
	t.Setenv(CacheHomeEnv, "")
	t.Setenv("HOME", "")
	got := Home()
	if got == "" {
		t.Fatal(`Home() = "", want a temp base directory`)
	}
	if got == filepath.Join(os.TempDir(), "fluxview-cache") {
		t.Errorf("Home() = %q, want an unpredictable path, not a squattable one", got)
	}
	if filepath.Dir(got) != tmp || !strings.HasPrefix(filepath.Base(got), "fluxview-cache-") {
		t.Errorf("Home() = %q, want a fresh fluxview-cache-* directory under %s", got, tmp)
	}
	if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
		t.Errorf("Home() = %q: want an existing directory (stat err: %v)", got, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(got) })
}

// resetWarnOnce clears the process-wide warn-once state: warn-once is per
// process, tests share one process (the same trick the helm tests use with
// their sync.Once vars).
func resetWarnOnce() {
	warnMu.Lock()
	defer warnMu.Unlock()
	warnOnce = make(map[string]*sync.Once)
}

// homeDir is the test's view of os.UserHomeDir.
func homeDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	return home
}

// syncBuffer is a threadsafe bytes.Buffer for capturing warnings.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
