package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/git"
	"github.com/banschikovde/fluxview/internal/gitsource"
)

// gitSourceEnv carries what the Kustomization pipeline needs to build from
// external GitRepository sources: the identity of the local repository
// (what "external" is measured against) and the fetcher that clones
// upstreams into the git source cache. A nil gitSourceEnv disables the
// feature — every Kustomization then resolves its path against the local
// repository exactly as before.
type gitSourceEnv struct {
	// originURL identifies the local repository ("", when unknown — then
	// nothing is external).
	originURL string
	// fetcher clones external upstreams; never nil on a non-nil env with
	// fetching enabled.
	fetcher *gitsource.Fetcher
	// noFetch disables all network fetching (--no-git-source-fetch): external
	// sources are still identified (their paths must not resolve against the
	// local repository), but ensure fails fast instead of cloning. The KS then
	// follows the ordinary fetch-failure path: warn + skip in build/diff,
	// gate failure in validate.
	noFetch bool
}

// originURLFor best-effort reads the local repository's origin URL. Any
// failure (not a git repo, no remotes, ambiguity) yields "" — external
// source detection is then off and behavior matches the pre-feature
// pipeline instead of failing the run.
func originURLFor(ctx context.Context, repoRoot string) string {
	ops, err := git.NewOperations(repoRoot)
	if err != nil {
		return ""
	}
	url, err := ops.OriginURL(ctx)
	if err != nil {
		return ""
	}
	return url
}

// newGitSourceEnv builds the env from the real repository root — even for
// builds that run in a temporary worktree (diff's comparison side), the
// identity must come from the repository that has the remotes. With noFetch
// the origin identity is still resolved (a local git operation, no network —
// lookupExternal needs it to keep external paths from resolving locally),
// but no fetcher is created.
func newGitSourceEnv(ctx context.Context, realRepoRoot string, ksCache kustomizeCacheOptions, noFetch bool) *gitSourceEnv {
	e := &gitSourceEnv{
		originURL: originURLFor(ctx, realRepoRoot),
		noFetch:   noFetch,
	}
	if !noFetch {
		e.fetcher = gitsource.NewFetcher(ksCache.gitSourceDir, ksCache.gitSourceTtl)
	}
	return e
}

// Close releases the env's resources: temporary clones made while the git
// source cache was disabled are removed (cached clones are kept). Safe on
// a nil env; call from a defer at the command level.
func (e *gitSourceEnv) Close() error {
	if e == nil {
		return nil
	}
	return e.fetcher.Close()
}

// lookupExternal returns the GitRepository a Kustomization's sourceRef
// points at when it is an external upstream (a different repository than
// the local origin). The sourceRef namespace defaults to the
// Kustomization's own namespace, as in Flux.
func (e *gitSourceEnv) lookupExternal(repos map[string]flux.GitRepository, ks flux.Kustomization) (flux.GitRepository, bool) {
	if e == nil || e.originURL == "" {
		return flux.GitRepository{}, false
	}
	if ks.Spec.SourceRef.Kind != flux.KindGitRepository || ks.Spec.SourceRef.Name == "" {
		return flux.GitRepository{}, false
	}
	ns := ks.Spec.SourceRef.Namespace
	if ns == "" {
		ns = ks.Metadata.Namespace
	}
	gr, ok := repos[ns+"/"+ks.Spec.SourceRef.Name]
	if !ok {
		return flux.GitRepository{}, false
	}
	if git.SameGitRepo(gr.Spec.URL, e.originURL) {
		return flux.GitRepository{}, false
	}
	return gr, true
}

// ensure fetches (or reuses) the local clone of an external GitRepository.
// With fetching disabled it fails before any network: the caller treats it
// exactly like an unreachable upstream. The message reads through the
// wrapper ("fetching <source> for <ks> failed: <message>") — keep it a bare
// cause, not a sentence of its own.
func (e *gitSourceEnv) ensure(ctx context.Context, gr flux.GitRepository) (string, error) {
	if e.noFetch {
		return "", fmt.Errorf("disabled by --no-git-source-fetch or %s", gitsource.EnvNoFetch)
	}
	return e.fetcher.Ensure(ctx, gr)
}

// describeSource names a Kustomization's source for warnings and the
// validation report ("GitRepository kyverno/kyverno" etc.).
func describeSource(ks flux.Kustomization) string {
	kind := ks.Spec.SourceRef.Kind
	if kind == "" {
		kind = "source"
	}
	ns := ks.Spec.SourceRef.Namespace
	if ns == "" {
		ns = ks.Metadata.Namespace
	}
	return fmt.Sprintf("%s %s/%s", kind, ns, ks.Spec.SourceRef.Name)
}

// registerNoGitSourceFetchFlag registers the external git source network
// kill switch shared by the commands. Lives here, next to the gitSourceEnv
// it toggles. Default from the environment, an explicit flag wins for the
// run.
func registerNoGitSourceFetchFlag(cmd *cobra.Command, noFetch *bool) {
	cmd.Flags().BoolVar(noFetch, "no-git-source-fetch", gitsource.DefaultNoFetch(),
		"Do not clone external GitRepository sources (no network at all): Kustomizations pointing at them are skipped with a warning, validate fails — for slow or offline networks (env: "+gitsource.EnvNoFetch+")")
}
