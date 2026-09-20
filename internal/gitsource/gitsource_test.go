package gitsource

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/banschikovde/fluxview/internal/flux"
)

// fixtureRepo is a local git repository used as a fetch upstream. Commits
// land on the default branch; tags are lightweight.
type fixtureRepo struct {
	t    *testing.T
	dir  string
	repo *gogit.Repository
}

func newFixtureRepo(t *testing.T) *fixtureRepo {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	return &fixtureRepo{t: t, dir: dir, repo: repo}
}

func (f *fixtureRepo) url() string {
	return "file://" + f.dir
}

// commit writes path with content, commits it and returns the commit sha.
func (f *fixtureRepo) commit(path, content string) string {
	f.t.Helper()
	filePath := filepath.Join(f.dir, path)
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		f.t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", path, err)
	}
	w, err := f.repo.Worktree()
	if err != nil {
		f.t.Fatalf("worktree: %v", err)
	}
	if _, err := w.Add(path); err != nil {
		f.t.Fatalf("add %s: %v", path, err)
	}
	hash, err := w.Commit("add "+path, &gogit.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@test.com", When: time.Now()},
	})
	if err != nil {
		f.t.Fatalf("commit: %v", err)
	}
	return hash.String()
}

func (f *fixtureRepo) tag(name string) {
	f.t.Helper()
	head, err := f.repo.Head()
	if err != nil {
		f.t.Fatalf("head: %v", err)
	}
	if _, err := f.repo.CreateTag(name, head.Hash(), nil); err != nil {
		f.t.Fatalf("tag %s: %v", name, err)
	}
}

// branch returns the default branch name of the fixture.
func (f *fixtureRepo) branch() string {
	f.t.Helper()
	head, err := f.repo.Head()
	if err != nil {
		f.t.Fatalf("head: %v", err)
	}
	return head.Name().Short()
}

func readCloneFile(t *testing.T, dir, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, path))
	if err != nil {
		t.Fatalf("reading %s from clone: %v", path, err)
	}
	return string(data)
}

func mustEnsure(t *testing.T, f *Fetcher, url string, ref *flux.GitRepositoryRef) string {
	t.Helper()
	dir, err := f.Ensure(context.Background(), flux.GitRepository{
		Metadata: flux.ObjectMeta{Name: "upstream", Namespace: "test"},
		Spec:     flux.GitRepositorySpec{URL: url, Ref: ref},
	})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	return dir
}

func TestEnsure_TagIsPinnedAndReused(t *testing.T) {
	up := newFixtureRepo(t)
	up.commit("config/crds/crd.yaml", "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n")
	up.tag("v1.0.0")
	up.commit("config/crds/crd.yaml", "changed later — must not appear\n")

	f := NewFetcher(t.TempDir(), time.Hour)
	ref := &flux.GitRepositoryRef{Tag: "v1.0.0"}

	first := mustEnsure(t, f, up.url(), ref)
	if got := readCloneFile(t, first, "config/crds/crd.yaml"); !strings.Contains(got, "CustomResourceDefinition") {
		t.Errorf("clone at tag holds wrong content: %q", got)
	}

	second := mustEnsure(t, f, up.url(), ref)
	if first != second {
		t.Errorf("pinned tag re-cloned or moved: %q then %q", first, second)
	}
}

func TestEnsure_CommitChecksOutExactSHA(t *testing.T) {
	up := newFixtureRepo(t)
	sha1 := up.commit("file.txt", "first\n")
	up.commit("file.txt", "second\n")

	f := NewFetcher(t.TempDir(), time.Hour)
	dir := mustEnsure(t, f, up.url(), &flux.GitRepositoryRef{Commit: sha1})
	if got := readCloneFile(t, dir, "file.txt"); got != "first\n" {
		t.Errorf("clone at commit sha holds %q, want the first commit", got)
	}

	if again := mustEnsure(t, f, up.url(), &flux.GitRepositoryRef{Commit: sha1}); again != dir {
		t.Errorf("pinned commit re-cloned: %q then %q", dir, again)
	}
}

func TestEnsure_BranchHonorsTTL(t *testing.T) {
	up := newFixtureRepo(t)
	up.commit("file.txt", "first\n")
	branch := up.branch()

	fresh := NewFetcher(t.TempDir(), time.Hour)
	ref := &flux.GitRepositoryRef{Branch: branch}
	first := mustEnsure(t, fresh, up.url(), ref)

	// Within the TTL the pointer is reused without re-resolution: the new
	// commit must not be visible even though the branch moved.
	up.commit("file.txt", "second\n")
	if got := readCloneFile(t, mustEnsure(t, fresh, up.url(), ref), "file.txt"); got != "first\n" {
		t.Errorf("fresh pointer must serve the old resolution, got %q", got)
	}
	if again := mustEnsure(t, fresh, up.url(), ref); again != first {
		t.Errorf("fresh resolution must reuse the same clone dir: %q vs %q", again, first)
	}

	// TTL 0 always re-resolves: the moved branch yields a new clone.
	expired := NewFetcher(t.TempDir(), 0)
	after := mustEnsure(t, expired, up.url(), ref)
	if got := readCloneFile(t, after, "file.txt"); got != "second\n" {
		t.Errorf("expired pointer must re-resolve to the moved branch, got %q", got)
	}
}

func TestEnsure_HeadNoRef(t *testing.T) {
	up := newFixtureRepo(t)
	up.commit("file.txt", "first\n")

	f := NewFetcher(t.TempDir(), 0)
	first := mustEnsure(t, f, up.url(), nil)
	if got := readCloneFile(t, first, "file.txt"); got != "first\n" {
		t.Errorf("HEAD clone holds %q", got)
	}

	up.commit("file.txt", "second\n")
	if again := mustEnsure(t, f, up.url(), nil); again == first {
		t.Error("no-ref HEAD must re-resolve with TTL 0, got the same clone dir")
	}

	// Within a TTL the HEAD resolution is cached like any floating ref.
	fresh := NewFetcher(t.TempDir(), time.Hour)
	cached := mustEnsure(t, fresh, up.url(), nil)
	if again := mustEnsure(t, fresh, up.url(), nil); again != cached {
		t.Errorf("fresh HEAD resolution must reuse the same clone dir: %q vs %q", again, cached)
	}
	if got := readCloneFile(t, cached, "file.txt"); got != "second\n" {
		t.Errorf("HEAD clone holds %q", got)
	}
}

func TestEnsure_SemverPicksHighestMatch(t *testing.T) {
	up := newFixtureRepo(t)
	up.commit("file.txt", "v1.0.0\n")
	up.tag("v1.0.0")
	up.commit("file.txt", "v1.1.0\n")
	up.tag("v1.1.0")
	up.commit("file.txt", "v2.0.0\n")
	up.tag("v2.0.0")

	f := NewFetcher(t.TempDir(), time.Hour)
	dir := mustEnsure(t, f, up.url(), &flux.GitRepositoryRef{Semver: ">=1.0.0 <2.0.0"})
	if got := readCloneFile(t, dir, "file.txt"); got != "v1.1.0\n" {
		t.Errorf("semver clone holds %q, want the v1.1.0 tag content", got)
	}

	// The picked tag is cached as a pinned resolution for the TTL.
	if again := mustEnsure(t, f, up.url(), &flux.GitRepositoryRef{Semver: ">=1.0.0 <2.0.0"}); again != dir {
		t.Errorf("semver resolution must be reused within the TTL: %q vs %q", again, dir)
	}
}

func TestEnsure_SemverNoMatch(t *testing.T) {
	up := newFixtureRepo(t)
	up.commit("file.txt", "x\n")
	up.tag("v0.9.0")

	f := NewFetcher(t.TempDir(), time.Hour)
	_, err := f.Ensure(context.Background(), flux.GitRepository{
		Metadata: flux.ObjectMeta{Name: "upstream", Namespace: "test"},
		Spec:     flux.GitRepositorySpec{URL: up.url(), Ref: &flux.GitRepositoryRef{Semver: ">=1.0.0"}},
	})
	if err == nil {
		t.Fatal("Ensure must fail when no tag matches the semver constraint")
	}
	for _, want := range []string{">=1.0.0", "v0.9.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %q", err, want)
		}
	}
}

func TestEnsure_OffCacheDirDisablesReuse(t *testing.T) {
	up := newFixtureRepo(t)
	up.commit("file.txt", "pinned\n")
	up.tag("v1.0.0")

	f := NewFetcher("off", time.Hour)
	defer f.Close()
	ref := &flux.GitRepositoryRef{Tag: "v1.0.0"}
	first := mustEnsure(t, f, up.url(), ref)
	second := mustEnsure(t, f, up.url(), ref)
	if first == second {
		t.Errorf("off cache must clone fresh every time, got the same dir %q twice", first)
	}
	if got := readCloneFile(t, second, "file.txt"); got != "pinned\n" {
		t.Errorf("off-cache clone holds %q", got)
	}

	// Close removes the off-mode temp clones — no leak in os.TempDir().
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(first)); !os.IsNotExist(err) {
		t.Errorf("off-mode clone base %s must be removed by Close, stat err = %v", filepath.Dir(first), err)
	}
	if err := f.Close(); err != nil {
		t.Errorf("Close must be safe twice, got: %v", err)
	}
}

func TestEnsure_BrokenSeedIsRepaired(t *testing.T) {
	up := newFixtureRepo(t)
	up.commit("file.txt", "content\n")
	up.tag("v1.0.0")

	f := NewFetcher(t.TempDir(), time.Hour)
	dir := mustEnsure(t, f, up.url(), &flux.GitRepositoryRef{Tag: "v1.0.0"})

	// Corrupt the seed — the directory is no longer a valid clone entry.
	if err := os.WriteFile(filepath.Join(dir, seedFileName), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupting seed: %v", err)
	}

	fixed := mustEnsure(t, f, up.url(), &flux.GitRepositoryRef{Tag: "v1.0.0"})
	if fixed != dir {
		t.Errorf("repaired clone must reuse the same key dir: %q vs %q", fixed, dir)
	}
	if !seedValid(dir) {
		t.Error("seed must be valid after the repair re-clone")
	}
}

func TestEnsure_MissingSeedFileReclones(t *testing.T) {
	up := newFixtureRepo(t)
	up.commit("file.txt", "content\n")
	up.tag("v1.0.0")

	f := NewFetcher(t.TempDir(), time.Hour)
	dir := mustEnsure(t, f, up.url(), &flux.GitRepositoryRef{Tag: "v1.0.0"})
	if err := os.Remove(filepath.Join(dir, seedFileName)); err != nil {
		t.Fatalf("removing seed: %v", err)
	}

	again := mustEnsure(t, f, up.url(), &flux.GitRepositoryRef{Tag: "v1.0.0"})
	if again != dir || !seedValid(dir) {
		t.Errorf("clone with a missing seed must be re-created in place: %q, seedValid=%v", again, seedValid(dir))
	}
}

func TestEnsure_ConcurrentSameKey(t *testing.T) {
	up := newFixtureRepo(t)
	up.commit("file.txt", "content\n")
	up.tag("v1.0.0")

	cacheDir := t.TempDir()
	f := NewFetcher(cacheDir, time.Hour)
	ref := &flux.GitRepositoryRef{Tag: "v1.0.0"}

	var wg sync.WaitGroup
	dirs := make([]string, 8)
	for i := range dirs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dir, err := f.Ensure(context.Background(), flux.GitRepository{
				Metadata: flux.ObjectMeta{Name: "upstream", Namespace: "test"},
				Spec:     flux.GitRepositorySpec{URL: up.url(), Ref: ref},
			})
			if err != nil {
				t.Errorf("concurrent Ensure: %v", err)
				return
			}
			dirs[i] = dir
		}(i)
	}
	wg.Wait()

	for i, dir := range dirs {
		if dir != dirs[0] {
			t.Fatalf("concurrent Ensures disagree on the dir: [%d]=%q vs %q", i, dir, dirs[0])
		}
	}

	// Exactly one immutable data directory, no temp leftovers.
	dataEntries, err := os.ReadDir(filepath.Join(cacheDir, "data"))
	if err != nil || len(dataEntries) != 1 {
		t.Errorf("data/ must hold exactly one clone after the race, got %d (err %v)", len(dataEntries), err)
	}
	if tmpEntries, _ := os.ReadDir(filepath.Join(cacheDir, "tmp")); len(tmpEntries) != 0 {
		t.Errorf("tmp/ must be empty after the race, got %d entries", len(tmpEntries))
	}
}

func TestEnsure_ConcurrentFloatingRefresh(t *testing.T) {
	up := newFixtureRepo(t)
	up.commit("file.txt", "first\n")
	branch := up.branch()

	// In-process: concurrent Ensures of a floating branch with TTL 0 (every
	// call re-resolves) must agree on one clone dir and leave no temp dirs.
	cacheDir := t.TempDir()
	f := NewFetcher(cacheDir, 0)
	ref := &flux.GitRepositoryRef{Branch: branch}

	var wg sync.WaitGroup
	dirs := make([]string, 4)
	for i := range dirs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dirs[i] = mustEnsure(t, f, up.url(), ref)
		}(i)
	}
	wg.Wait()
	for i, dir := range dirs {
		if dir != dirs[0] {
			t.Fatalf("concurrent floating Ensures disagree: [%d]=%q vs %q", i, dir, dirs[0])
		}
	}

	// Cross-process analog: separate Fetchers over one shared cache dir.
	shared := t.TempDir()
	results := make([]string, 4)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ff := NewFetcher(shared, 0)
			defer ff.Close()
			results[i] = mustEnsure(t, ff, up.url(), ref)
		}(i)
	}
	wg.Wait()
	for i, dir := range results {
		if dir != results[0] {
			t.Fatalf("Fetchers sharing a cache dir disagree: [%d]=%q vs %q", i, dir, results[0])
		}
	}
	dataEntries, err := os.ReadDir(filepath.Join(shared, "data"))
	if err != nil || len(dataEntries) != 1 {
		t.Errorf("shared cache must hold exactly one clone, got %d (err %v)", len(dataEntries), err)
	}
	if tmpEntries, _ := os.ReadDir(filepath.Join(shared, "tmp")); len(tmpEntries) != 0 {
		t.Errorf("tmp/ must be empty, got %d entries", len(tmpEntries))
	}
}

func TestEnsure_FetchErrorNamesURL(t *testing.T) {
	f := NewFetcher(t.TempDir(), time.Hour)
	_, err := f.Ensure(context.Background(), flux.GitRepository{
		Metadata: flux.ObjectMeta{Name: "upstream", Namespace: "test"},
		Spec:     flux.GitRepositorySpec{URL: "file://" + filepath.Join(t.TempDir(), "missing-repo")},
	})
	if err == nil {
		t.Fatal("Ensure on a missing repository must fail")
	}
	if !strings.Contains(err.Error(), "missing-repo") {
		t.Errorf("error must name the URL: %v", err)
	}
}

func TestEnsure_EmptyURL(t *testing.T) {
	f := NewFetcher(t.TempDir(), time.Hour)
	_, err := f.Ensure(context.Background(), flux.GitRepository{
		Metadata: flux.ObjectMeta{Name: "upstream", Namespace: "test"},
	})
	if err == nil || !strings.Contains(err.Error(), "spec.url") {
		t.Errorf("Ensure without spec.url must fail naming the field, got %v", err)
	}
}

func TestEnsure_ContextCancellation(t *testing.T) {
	up := newFixtureRepo(t)
	up.commit("file.txt", "content\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := NewFetcher(t.TempDir(), time.Hour)
	_, err := f.Ensure(ctx, flux.GitRepository{
		Metadata: flux.ObjectMeta{Name: "upstream", Namespace: "test"},
		Spec:     flux.GitRepositorySpec{URL: up.url(), Ref: &flux.GitRepositoryRef{Branch: up.branch()}},
	})
	if err == nil {
		t.Fatal("Ensure with a cancelled context must fail")
	}
}

func TestPickSemverTag(t *testing.T) {
	refs := func(names ...string) []*plumbing.Reference {
		out := make([]*plumbing.Reference, 0, len(names))
		for _, n := range names {
			out = append(out, plumbing.NewReferenceFromStrings(n, "0000000000000000000000000000000000000001"))
		}
		return out
	}

	tests := []struct {
		name       string
		refs       []*plumbing.Reference
		constraint string
		want       string
		wantErr    bool
	}{
		{"highest match", refs("refs/tags/v1.0.0", "refs/tags/v1.1.0", "refs/tags/v2.0.0"), ">=1.0.0 <2.0.0", "v1.1.0", false},
		{"no v prefix", refs("refs/tags/1.0.0", "refs/tags/1.2.0"), "^1", "1.2.0", false},
		{"v-prefix and bare mixed", refs("refs/tags/v1.0.0", "refs/tags/1.3.0"), ">=1.0.0", "1.3.0", false},
		{"prerelease excluded without -0", refs("refs/tags/v1.0.0", "refs/tags/v1.1.0-rc.1"), ">=1.0.0", "v1.0.0", false},
		{"prerelease allowed with -0", refs("refs/tags/v1.1.0-rc.1"), ">=1.0.0-0", "v1.1.0-rc.1", false},
		{"peeled entries ignored", refs("refs/tags/v1.0.0", "refs/tags/v1.0.0^{}", "refs/tags/v1.2.0^{}"), ">=1.0.0", "v1.0.0", false},
		{"non-semver tags ignored", refs("refs/tags/latest", "refs/tags/v1.0.0"), "*", "v1.0.0", false},
		{"no match", refs("refs/tags/v0.1.0"), ">=1.0.0", "", true},
		{"no tags at all", refs("refs/heads/main"), ">=1.0.0", "", true},
		{"invalid constraint", refs("refs/tags/v1.0.0"), "not-a-constraint", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pickSemverTag(tt.refs, tt.constraint)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("pickSemverTag(%q) = %q, want error", tt.constraint, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("pickSemverTag(%q): %v", tt.constraint, err)
			}
			if got != tt.want {
				t.Errorf("pickSemverTag(%q) = %q, want %q", tt.constraint, got, tt.want)
			}
		})
	}
}

func TestCacheKey_NormalizesURLSpellings(t *testing.T) {
	a := cacheKey("https://github.com/kyverno/kyverno.git", "tag:v1.0.0")
	b := cacheKey("git@github.com:kyverno/kyverno", "tag:v1.0.0")
	if a != b {
		t.Errorf("https and scp spellings of one repo must share a cache key:\n%s\n%s", a, b)
	}
	if c := cacheKey("https://github.com/kyverno/kyverno.git", "tag:v1.1.0"); c == a {
		t.Error("different refs must not collide on a cache key")
	}
}

func TestFetcher_DisabledFetcherStillFetches(t *testing.T) {
	up := newFixtureRepo(t)
	up.commit("file.txt", "pinned\n")
	up.tag("v1.0.0")

	// A nil Fetcher must not silently skip: it fetches to a temp dir.
	var f *Fetcher
	dir, err := f.Ensure(context.Background(), flux.GitRepository{
		Metadata: flux.ObjectMeta{Name: "upstream", Namespace: "test"},
		Spec:     flux.GitRepositorySpec{URL: up.url(), Ref: &flux.GitRepositoryRef{Tag: "v1.0.0"}},
	})
	if err != nil {
		t.Fatalf("nil Fetcher Ensure: %v", err)
	}
	if got := readCloneFile(t, dir, "file.txt"); got != "pinned\n" {
		t.Errorf("nil Fetcher clone holds %q", got)
	}
}

func TestEnsure_SSHResolutionErrorFailsBeforeNetwork(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	emptyKnownHosts(t)

	f := NewFetcher(t.TempDir(), time.Hour)
	_, err := f.Ensure(context.Background(), flux.GitRepository{
		Metadata: flux.ObjectMeta{Name: "upstream", Namespace: "test"},
		Spec:     flux.GitRepositorySpec{URL: "ssh://git@git.example.com/platform/crds.git"},
	})
	if err == nil || !strings.Contains(err.Error(), "no SSH credentials found for git@git.example.com:22") {
		t.Fatalf("Ensure must surface the auth resolution error, got: %v", err)
	}
}

// closedPort returns a loopback host:port that just stopped listening, so
// a connect attempt fails fast with connection refused (offline test of
// the transport path).
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func TestEnsure_SSHAuthThreadsToTransport(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	emptyKnownHosts(t)
	keyPath := writeKeyFile(t, filepath.Join(t.TempDir(), "id_custom"), mustPriv(t), "")
	t.Setenv(envSSHKey, keyPath)

	url := "ssh://git@" + closedPort(t) + "/platform/crds.git"
	f := NewFetcher(t.TempDir(), time.Hour)
	_, err := f.Ensure(context.Background(), flux.GitRepository{
		Metadata: flux.ObjectMeta{Name: "upstream", Namespace: "test"},
		Spec:     flux.GitRepositorySpec{URL: url, Ref: &flux.GitRepositoryRef{Branch: "main"}},
	})
	if err == nil {
		t.Fatal("Ensure against a closed port must fail")
	}
	// The URL+ref wrapper is preserved and the failure is the transport's
	// (connection refused), not an auth-resolution one: the resolved key
	// and host key callback were handed to the dialer.
	for _, want := range []string{url, "connection refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must mention %q", err, want)
		}
	}
}

func TestFetcher_CloseClosesAgentConnection(t *testing.T) {
	clearAuthEnv(t)
	t.Setenv("HOME", t.TempDir())
	emptyKnownHosts(t)
	sock, live := startTestAgent(t, true)
	t.Setenv("SSH_AUTH_SOCK", sock)

	f := NewFetcher(t.TempDir(), time.Hour)
	if _, err := f.resolveAuthFor("git@git.example.com:crds.git"); err != nil {
		t.Fatalf("resolveAuthFor: %v", err)
	}
	waitFor(t, "agent dialed through the Fetcher", func() bool { return live.Load() == 1 })

	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, "agent connection closed by Fetcher.Close", func() bool { return live.Load() == 0 })
}

func TestExplainAuthFailure(t *testing.T) {
	sshRejected := errors.New("ssh: handshake failed: ssh: unable to authenticate, attempted methods [publickey], no supported methods remain")

	t.Run("ssh rejected names the credential source", func(t *testing.T) {
		res := authOutcome{ssh: true, keySource: "ssh-agent"}
		err := explainAuthFailure(res, sshRejected)
		for _, want := range []string{"SSH authentication failed", "ssh-agent"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q must mention %q", err, want)
			}
		}
		if !strings.Contains(err.Error(), "unable to authenticate") {
			t.Errorf("the transport error must stay in the message: %v", err)
		}
	})

	t.Run("http 401 suggests credentials", func(t *testing.T) {
		res := authOutcome{}
		err := explainAuthFailure(res, fmt.Errorf("listing refs: %w", transport.ErrAuthenticationRequired))
		for _, want := range []string{"private repository or bad credentials", envGitUsername, ".netrc"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q must mention %q", err, want)
			}
		}
	})

	t.Run("http 403 too", func(t *testing.T) {
		res := authOutcome{}
		err := explainAuthFailure(res, fmt.Errorf("cloning: %w", transport.ErrAuthorizationFailed))
		if !strings.Contains(err.Error(), "private repository or bad credentials") {
			t.Errorf("error %q must name the auth problem", err)
		}
	})

	t.Run("other errors pass through", func(t *testing.T) {
		plain := errors.New("dial tcp: connection refused")
		for _, res := range []authOutcome{
			{ssh: true, keySource: "ssh-agent"},
			{},
		} {
			if got := explainAuthFailure(res, plain); got != plain {
				t.Errorf("non-auth error must pass through unchanged, got: %v", got)
			}
		}
	})

	t.Run("resolution-error outcomes never decorate", func(t *testing.T) {
		res := authOutcome{ssh: true, err: errors.New("no SSH credentials found")}
		if got := explainAuthFailure(res, sshRejected); got != sshRejected {
			t.Errorf("a failed resolution must not decorate transport errors, got: %v", got)
		}
	})
}
