package cli

import (
	"context"
	"fmt"

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
	// fetcher clones external upstreams; never nil on a non-nil env.
	fetcher *gitsource.Fetcher
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
// identity must come from the repository that has the remotes.
func newGitSourceEnv(ctx context.Context, realRepoRoot string, ksCache kustomizeCacheOptions) *gitSourceEnv {
	return &gitSourceEnv{
		originURL: originURLFor(ctx, realRepoRoot),
		fetcher:   gitsource.NewFetcher(ksCache.gitSourceDir, ksCache.gitSourceTtl),
	}
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
func (e *gitSourceEnv) ensure(ctx context.Context, gr flux.GitRepository) (string, error) {
	return e.fetcher.Ensure(ctx, gr)
}

// indexGitRepositories indexes GitRepositories by "namespace/name".
func indexGitRepositories(repos []flux.GitRepository) map[string]flux.GitRepository {
	if len(repos) == 0 {
		return nil
	}
	m := make(map[string]flux.GitRepository, len(repos))
	for _, gr := range repos {
		m[gr.Metadata.Namespace+"/"+gr.Metadata.Name] = gr
	}
	return m
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
