package sourcecache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestKeyIsDeterministicAndPartSeparated(t *testing.T) {
	a := Key("oci://x/y", "tag:v1")
	b := Key("oci://x/y", "tag:v1")
	if a != b {
		t.Errorf("Key must be deterministic: %q vs %q", a, b)
	}
	// NUL joining: ("ab", "c") must never equal ("a", "bc").
	if Key("ab", "c") == Key("a", "bc") {
		t.Error("Key parts must be NUL-separated, not concatenated")
	}
	if len(a) != 64 {
		t.Errorf("Key must be a sha256 hex string, got %d chars", len(a))
	}
}

func TestSeedRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := Meta{URL: "oci://reg/repo", Ref: "tag:v1", Resolved: "digest:sha256:abc", FetchedAt: time.Now().Format(time.RFC3339)}
	if err := WriteSeed(dir, "seed.json", m); err != nil {
		t.Fatalf("WriteSeed: %v", err)
	}
	if !SeedValid(dir, "seed.json") {
		t.Fatal("SeedValid must accept a fresh seed")
	}
}

func TestSeedValidRejects(t *testing.T) {
	dir := t.TempDir()
	if SeedValid(dir, "seed.json") {
		t.Error("a missing seed is not valid")
	}
	if err := os.WriteFile(filepath.Join(dir, "seed.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if SeedValid(dir, "seed.json") {
		t.Error("a malformed seed is not valid")
	}
	if err := os.WriteFile(filepath.Join(dir, "seed.json"), []byte(`{"url":"u"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if SeedValid(dir, "seed.json") {
		t.Error("a seed without a resolution is not valid")
	}
}

func TestFresh(t *testing.T) {
	now := time.Now().Format(time.RFC3339)
	if Fresh(0, Meta{FetchedAt: now}) {
		t.Error("ttl 0 is never fresh")
	}
	if Fresh(-time.Second, Meta{FetchedAt: now}) {
		t.Error("a negative ttl is never fresh")
	}
	if !Fresh(time.Hour, Meta{FetchedAt: now}) {
		t.Error("a just-written pointer is fresh for an hour")
	}
	if Fresh(time.Hour, Meta{FetchedAt: time.Now().Add(-2 * time.Hour).Format(time.RFC3339)}) {
		t.Error("an expired pointer is not fresh")
	}
	if Fresh(time.Hour, Meta{FetchedAt: "yesterday"}) {
		t.Error("an unparsable timestamp is not fresh")
	}
}

func TestPointerRoundTrip(t *testing.T) {
	cache := t.TempDir()
	m := Meta{URL: "u", Ref: "tag:v1", Resolved: "digest:sha256:abc", FetchedAt: time.Now().Format(time.RFC3339)}
	WritePointer(cache, "key1", m)
	got, ok := ReadPointer(cache, "key1")
	if !ok || got.Resolved != m.Resolved {
		t.Fatalf("ReadPointer: ok=%v got=%+v", ok, got)
	}
	// Unknown key and empty resolutions read as absent.
	if _, ok := ReadPointer(cache, "missing"); ok {
		t.Error("an unknown key must read as absent")
	}
	if err := os.WriteFile(filepath.Join(cache, "refs", "bad.json"), []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadPointer(cache, "bad"); ok {
		t.Error("a malformed pointer must read as absent")
	}
}

func TestDedupSingleFlightAndRetry(t *testing.T) {
	d := NewDedup()
	ctx := context.Background()

	single := false
	err := d.Do(ctx, "k", func() bool { return false }, func() error {
		if single {
			t.Error("fn ran twice without a retry")
		}
		single = true
		return nil
	})
	if err != nil || !single {
		t.Fatalf("first run: err=%v single=%v", err, single)
	}

	// A failed winner makes the waiter retry the work itself.
	var runs int
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = d.Do(ctx, "k2", func() bool { return false }, func() error {
			mu.Lock()
			runs++
			mu.Unlock()
			return nil
		})
	}()
	go func() {
		defer wg.Done()
		_ = d.Do(ctx, "k2", func() bool { return true }, func() error {
			return errors.New("should not run: done() already true")
		})
	}()
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if runs > 1 {
		t.Errorf("a completed winner must satisfy the waiter without a rerun, runs=%d", runs)
	}
}

func TestDedupWaiterRetriesAfterFailure(t *testing.T) {
	d := NewDedup()
	ctx := context.Background()

	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = d.Do(ctx, "k", func() bool { return false }, func() error {
			close(started)
			<-release
			return errors.New("fetch failed")
		})
	}()
	<-started

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- d.Do(ctx, "k", func() bool { return true }, func() error {
			return nil // not called: done() is true after the failed winner
		})
	}()
	// Give the waiter a moment to block on the winner, then fail it.
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-waitErr; err != nil {
		t.Fatalf("waiter after a failed winner: %v", err)
	}
}

func TestDedupContextCancelWhileWaiting(t *testing.T) {
	d := NewDedup()
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = d.Do(context.Background(), "k", func() bool { return false }, func() error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- d.Do(ctx, "k", func() bool { return false }, func() error { return nil })
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled waiter must surface ctx.Err(), got %v", err)
	}
	close(release)
}

func TestPublishReusesWinnerAndReplacesBroken(t *testing.T) {
	cache := t.TempDir()
	dir := filepath.Join(cache, "data", "key")
	tmp := filepath.Join(cache, "tmp", "fetch-1")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "f.txt"), []byte("winner"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Publish(tmp, dir, func(string) bool { return false }); err != nil {
		t.Fatalf("clean publish: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "f.txt")); err != nil {
		t.Fatalf("published file missing: %v", err)
	}

	// A rename onto the valid target: the loser's tmp is dropped.
	tmp2 := filepath.Join(cache, "tmp", "fetch-2")
	if err := os.MkdirAll(tmp2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp2, "f.txt"), []byte("loser"), 0o644); err != nil {
		t.Fatal(err)
	}
	valid := func(d string) bool {
		data, err := os.ReadFile(filepath.Join(d, "f.txt"))
		return err == nil && string(data) == "winner"
	}
	if err := Publish(tmp2, dir, valid); err != nil {
		t.Fatalf("loser publish: %v", err)
	}
	if _, err := os.Stat(tmp2); !os.IsNotExist(err) {
		t.Error("the loser's tmp must be removed")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(data) != "winner" {
		t.Error("the winner's content must stay")
	}

	// A rename onto a broken target: replaced.
	tmp3 := filepath.Join(cache, "tmp", "fetch-3")
	if err := os.MkdirAll(tmp3, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp3, "f.txt"), []byte("fresh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Publish(tmp3, dir, func(string) bool { return false }); err != nil {
		t.Fatalf("replace-broken publish: %v", err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "f.txt"))
	if string(data) != "fresh" {
		t.Errorf("a broken target must be replaced, got %q", data)
	}
}

func TestTTLFromEnv(t *testing.T) {
	t.Setenv("TTL_TEST_ENV", "")
	if got := TTLFromEnv("TTL_TEST_ENV"); got != 10*time.Minute {
		t.Errorf("unset env must yield the 10m default, got %s", got)
	}
	t.Setenv("TTL_TEST_ENV", "90s")
	if got := TTLFromEnv("TTL_TEST_ENV"); got != 90*time.Second {
		t.Errorf("a valid duration must pass through, got %s", got)
	}
	t.Setenv("TTL_TEST_ENV", "nonsense")
	if got := TTLFromEnv("TTL_TEST_ENV"); got != 10*time.Minute {
		t.Errorf("an invalid value must fall back to the default, got %s", got)
	}
}

func TestNoFetchFromEnv(t *testing.T) {
	for _, v := range []string{"1", "true", "YES", " on "} {
		t.Setenv("SWITCH_TEST_ENV", v)
		if !NoFetchFromEnv("SWITCH_TEST_ENV") {
			t.Errorf("%q must read as on", v)
		}
	}
	for _, v := range []string{"", "0", "false", "maybe"} {
		t.Setenv("SWITCH_TEST_ENV", v)
		if NoFetchFromEnv("SWITCH_TEST_ENV") {
			t.Errorf("%q must read as off", v)
		}
	}
}

func TestTempBaseLifecycle(t *testing.T) {
	var tb TempBase
	if err := tb.Remove(); err != nil {
		t.Fatalf("Remove before any Dir must be a no-op: %v", err)
	}
	dir, err := tb.Dir("fluxview-test-*")
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	again, err := tb.Dir("fluxview-test-*")
	if err != nil || again != dir {
		t.Fatalf("Dir must be memoized: %q vs %q (err %v)", again, dir, err)
	}
	if err := tb.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("Remove must delete the directory")
	}
}

func TestRememberPointer(t *testing.T) {
	cache := t.TempDir()
	// The fresh-from-pointer and self-resolved cases write nothing.
	RememberPointer(cache, "k", "u", "tag:v1", "digest:sha256:x", true)
	RememberPointer(cache, "k", "u", "digest:sha256:x", "digest:sha256:x", false)
	if _, ok := ReadPointer(cache, "k"); ok {
		t.Error("neither a fresh nor a self-resolved resolution writes a pointer")
	}
	RememberPointer(cache, "k", "u", "tag:v1", "digest:sha256:x", false)
	if m, ok := ReadPointer(cache, "k"); !ok || m.Resolved != "digest:sha256:x" {
		t.Fatalf("pointer not recorded: ok=%v m=%+v", ok, m)
	}
	RememberPointer("", "k", "u", "tag:v1", "digest:sha256:x", false) // no-op, no panic
}

func TestScratchDir(t *testing.T) {
	cache := t.TempDir()
	dir, err := ScratchDir(cache, "fetch-*")
	if err != nil {
		t.Fatalf("ScratchDir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("scratch dir missing: %v", err)
	}
	rel, err := filepath.Rel(cache, dir)
	if err != nil || filepath.Dir(rel) != "tmp" {
		t.Errorf("scratch dir must live under <cache>/tmp, got %s (rel %s)", dir, rel)
	}
}
