package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	diffpkg "github.com/banschikovde/fluxview/internal/diff"
	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/fsx"
	"github.com/banschikovde/fluxview/internal/git"
	"github.com/banschikovde/fluxview/internal/helm"
	"github.com/banschikovde/fluxview/internal/kustomize"
	"github.com/banschikovde/fluxview/internal/yamlutil"
	"github.com/cyphar/filepath-securejoin"
	"gopkg.in/yaml.v3"
)

// errAlreadyWarned indicates buildDirCached already printed a warning for
// this directory. Callers should skip their own follow-up warning.
var errAlreadyWarned = errors.New("build already warned")

// sectionSeparator joins consecutive YAML documents in the built output:
// newline, the --- document separator, newline.
const sectionSeparator = "\n---\n"

// DiffFlags holds flags for the diff command.
type DiffFlags struct {
	Path                string
	Namespace           string
	Color               string
	BranchOrig          string
	Unified             int
	SkipCRDs            bool
	StripAttrs          string
	HelmCacheDir        string
	HelmIndexTTL        time.Duration
	HelmDownloadTimeout time.Duration
	// KsCache groups the kustomize cache flags (remote resources, build
	// outputs, external git source clones) — populated by
	// registerKustomizeCacheFlags.
	KsCache kustomizeCacheOptions
	// GitSourceSSHKnownHosts and GitSourceSSHAcceptNew are the external
	// git source auth policy knobs (credentials stay env-only).
	GitSourceSSHKnownHosts string
	GitSourceSSHAcceptNew  bool
	// NoGitSourceFetch disables cloning external GitRepository sources
	// (--no-git-source-fetch): the network kill switch for slow links.
	NoGitSourceFetch bool
}

func newDiffCmd() *cobra.Command {
	flags := &DiffFlags{}

	cmd := &cobra.Command{
		Use:   "diff <resource> [name] [flags]",
		Short: "Compare Flux resources against another git revision",
		Long: `Compare Flux Kustomization or HelmRelease resources against another
git revision/branch and show the differences.

Resource types:
  ks, kustomization   — diff kustomize build output
  hr, helmrelease     — diff helm template output

If [name] is omitted, all resources of the type are compared.

Examples:
  fluxview diff ks --path clusters/prod/
  fluxview diff hr --path clusters/prod/
  fluxview diff ks --path clusters/dev/ --branch-orig main --strip-attrs helm.sh/chart,status --skip-crds`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			applyGitSourceAuthFlags(cmd.Flags(), flags.GitSourceSSHKnownHosts, flags.GitSourceSSHAcceptNew)
			return runDiff(cmd.Context(), args, flags)
		},
	}

	cmd.Flags().StringVarP(&flags.Path, "path", "p", "", "Path to the cluster directory in the repository")
	cmd.Flags().StringVarP(&flags.Namespace, "namespace", "n", "", "Filter output resources by namespace")
	cmd.Flags().StringVar(&flags.Color, "color", "auto", "Color mode: auto, always, never")
	cmd.Flags().StringVar(&flags.BranchOrig, "branch-orig", "", "Branch/revision to compare against (default: auto-detect default branch)")
	cmd.Flags().IntVar(&flags.Unified, "unified", 3, "Number of context lines in diff output")
	cmd.Flags().BoolVar(&flags.SkipCRDs, "skip-crds", false, "Skip CustomResourceDefinition resources in diff")
	cmd.Flags().StringVar(&flags.StripAttrs, "strip-attrs", "", "Comma-separated keys to strip from diff (e.g. helm.sh/chart,status)")
	registerHelmCacheFlags(cmd, &flags.HelmCacheDir, &flags.HelmIndexTTL, &flags.HelmDownloadTimeout)
	registerKustomizeCacheFlags(cmd, &flags.KsCache)
	registerGitSourceAuthFlags(cmd, &flags.GitSourceSSHKnownHosts, &flags.GitSourceSSHAcceptNew)
	registerNoGitSourceFetchFlag(cmd, &flags.NoGitSourceFetch)
	return cmd
}

func runDiff(ctx context.Context, args []string, flags *DiffFlags) error {
	if len(args) == 0 {
		return NewExitError(fmt.Errorf("resource type required (use 'ks' or 'hr')"), ExitCodeError)
	}

	resourceType := args[0]
	var name string
	if len(args) > 1 {
		name = args[1]
	}

	// Determine the cluster path.
	clusterPath := flags.Path
	if clusterPath == "" {
		clusterPath = "."
	}

	// Resolve to absolute path.
	absClusterPath, err := filepath.Abs(clusterPath)
	if err != nil {
		return NewExitError(fmt.Errorf("resolving path %s: %w", clusterPath, err), ExitCodeError)
	}

	// Verify the cluster path exists.
	if _, err := os.Stat(absClusterPath); os.IsNotExist(err) {
		return NewExitError(fmt.Errorf("path %s does not exist", clusterPath), ExitCodeError)
	}

	// Determine the repository root from the cluster path (not CWD).
	repoRoot, err := git.FindRepoRoot(absClusterPath)
	if err != nil {
		return NewExitError(fmt.Errorf("finding git repo root for %s: %w", clusterPath, err), ExitCodeError)
	}

	// Resolve the branch/revision to compare against.
	gitOps, err := git.NewOperations(repoRoot)
	if err != nil {
		return NewExitError(fmt.Errorf("initializing git operations: %w", err), ExitCodeError)
	}

	compareRevision := flags.BranchOrig
	if compareRevision == "" {
		// Auto-detect the default branch.
		compareRevision, err = gitOps.DefaultBranch(ctx)
		if err != nil {
			return NewExitError(fmt.Errorf("could not determine default branch (use --branch-orig): %w", err), ExitCodeError)
		}
		fmt.Fprintf(os.Stderr, "Comparing against auto-detected default branch: %s\n", compareRevision)
	} else {
		fmt.Fprintf(os.Stderr, "Comparing against revision: %s\n", compareRevision)
	}

	// Resolve revision to a commit hash.
	compareCommit, err := gitOps.ResolveRevision(ctx, compareRevision)
	if err != nil {
		return NewExitError(fmt.Errorf("resolving revision %s: %w", compareRevision, err), ExitCodeError)
	}

	switch resourceType {
	case "ks", "kustomization":
		return runDiffKS(ctx, gitOps, absClusterPath, repoRoot, name, compareCommit, flags)
	case "hr", "helmrelease":
		return runDiffHR(ctx, gitOps, absClusterPath, repoRoot, name, compareCommit, flags)
	default:
		return NewExitError(fmt.Errorf("unsupported resource type %q (use 'ks'/'kustomization' or 'hr'/'helmrelease')", resourceType), ExitCodeError)
	}
}
func runDiffKS(ctx context.Context, gitOps *git.Operations, clusterPath, repoRoot, name, compareCommit string, flags *DiffFlags) error {
	// External source identity comes from the real repository (the git ops
	// handle), never from the comparison worktree — it has no remotes.
	gitEnv := newGitSourceEnv(ctx, repoRoot, flags.KsCache, flags.NoGitSourceFetch)
	defer gitEnv.Close()
	env := &ksPipelineEnv{scans: newScanCache(), ksCache: flags.KsCache, gitSources: gitEnv}

	currentOutput, err := buildKSOutput(ctx, env, clusterPath, repoRoot, name)
	if err != nil {
		return NewExitError(fmt.Errorf("building current state: %w", err), ExitCodeError)
	}

	compareOutput, err := buildKSOutputAtRevision(ctx, env, revisionRef{ops: gitOps, revision: compareCommit}, clusterPath, repoRoot, name)
	if err != nil {
		return NewExitError(fmt.Errorf("building comparison state at %s: %w", compareCommit, err), ExitCodeError)
	}

	return computeAndOutputDiff(ctx, compareOutput, currentOutput, flags)
}

func runDiffHR(ctx context.Context, gitOps *git.Operations, clusterPath, repoRoot, name, compareCommit string, flags *DiffFlags) error {
	// Current state.
	hasDirectKS, err := hasDirectKustomizations(clusterPath)
	if err != nil {
		return NewExitError(fmt.Errorf("checking for Kustomization files: %w", err), ExitCodeError)
	}
	if !hasDirectKS {
		return NewExitError(fmt.Errorf("no Kustomization files found in %s", clusterPath), ExitCodeError)
	}
	// Diff mode is strict: a HelmRelease that fails to inflate (e.g. chart
	// download impossible) on either side must fail the diff — otherwise the
	// side silently missing its resources would show up as a false
	// "added" (green) / "removed" (red) diff.
	scans := newScanCache()
	// One git source env for both diff sides: the same origin identity, and
	// the shared clone cache keeps pinned external sources byte-identical
	// across the comparison (floating refs honor the cache TTL).
	gitEnv := newGitSourceEnv(ctx, repoRoot, flags.KsCache, flags.NoGitSourceFetch)
	defer gitEnv.Close()
	env := &hrInflationEnv{
		scans:      scans,
		strict:     true, // a side that cannot inflate must fail the diff
		helmCache:  helmCacheOptions{dir: flags.HelmCacheDir, indexTTL: flags.HelmIndexTTL, downloadTimeout: flags.HelmDownloadTimeout},
		ksCache:    flags.KsCache,
		gitSources: gitEnv,
	}
	currentOutput, err := buildHRInflation(ctx, env, clusterPath, repoRoot, name, flags.Namespace, false)
	if err != nil {
		return NewExitError(fmt.Errorf("building current state: %w", err), ExitCodeError)
	}

	// Comparison state at revision.
	worktreePath, err := gitOps.CloneToDir(ctx, compareCommit)
	if err != nil {
		return NewExitError(fmt.Errorf("creating worktree at %s: %w", compareCommit, err), ExitCodeError)
	}
	defer gitOps.RemoveWorktree(ctx, worktreePath)

	relPath, err := filepath.Rel(repoRoot, clusterPath)
	if err != nil {
		relPath = clusterPath
	}
	worktreeClusterPath := filepath.Join(worktreePath, relPath)

	if _, err := os.Stat(worktreeClusterPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Warning: path %s does not exist at revision %s\n", relPath, compareCommit)
	} else {
		compareOutput, err := buildHRInflation(ctx, env, worktreeClusterPath, worktreePath, name, flags.Namespace, true)
		if err != nil {
			// A HelmRelease selected by name may legitimately not exist at the
			// comparison revision (added in this branch) — that's a valid
			// "added" diff, not an error. Any other failure means this side's
			// state could not be built; the diff would be incomplete and
			// misleading, so fail instead.
			if errors.Is(err, errHRNotFound) {
				compareOutput = nil
			} else {
				return NewExitError(fmt.Errorf("building comparison state at %s: %w", compareCommit, err), ExitCodeError)
			}
		}
		return computeAndOutputDiff(ctx, compareOutput, currentOutput, flags)
	}

	return computeAndOutputDiff(ctx, nil, currentOutput, flags)
}

// ksPipelineEnv is the per-command state of the KS diff pipeline: memoized
// scans, the kustomize cache settings and the shared external git source env.
type ksPipelineEnv struct {
	scans      *scanCache
	ksCache    kustomizeCacheOptions
	gitSources *gitSourceEnv
}

// revisionRef points at the git state a comparison build runs against.
type revisionRef struct {
	ops      *git.Operations
	revision string
}

// buildKSOutput builds the Kustomization output for the current working tree.
func buildKSOutput(ctx context.Context, env *ksPipelineEnv, clusterPath, repoRoot, name string) ([]byte, error) {
	scans, gitSources := env.scans, env.gitSources
	// Check that the path contains Kustomization files directly (not just in subdirectories)
	hasDirectKS, err := hasDirectKustomizations(clusterPath)
	if err != nil {
		return nil, fmt.Errorf("checking for Kustomization files: %w", err)
	}
	if !hasDirectKS {
		return nil, fmt.Errorf("no Kustomization files found in %s", clusterPath)
	}

	parser := scans.parserFor(clusterPath)
	kustomizations, err := parser.ParseKustomizations(ctx)
	if err != nil {
		return nil, fmt.Errorf("parsing Kustomization resources: %w", err)
	}

	if name != "" {
		kustomizations = filterKustomizations(kustomizations, name)
		if len(kustomizations) == 0 {
			return nil, fmt.Errorf("kustomization %q not found", name)
		}
	}

	builder := kustomize.NewBuilder(repoRoot, env.ksCache.builderOptions()...)
	buildCache := make(buildCache)
	// Resolve ConfigMaps and Secrets for postBuild substitution.
	configMaps := resolveConfigMaps(ctx, scans, clusterPath, builder, buildCache)
	secrets := resolveSecrets(ctx, scans, clusterPath, builder, buildCache)

	return buildKSContent(ctx, &ksBuildEnv{
		scans:       scans,
		builder:     builder,
		repoRoot:    repoRoot,
		clusterPath: clusterPath,
		quiet:       false,
		cache:       buildCache,
		gitSources:  gitSources,
	}, kustomizations, substitutionSources{configMaps: configMaps, secrets: secrets})
}

// buildKSOutputAtRevision builds the Kustomization output at a specific git revision.
func buildKSOutputAtRevision(ctx context.Context, env *ksPipelineEnv, rev revisionRef, clusterPath, repoRoot, name string) ([]byte, error) {
	scans, gitSources := env.scans, env.gitSources
	gitOps, revision := rev.ops, rev.revision
	// Create a git worktree at the target revision.
	worktreePath, err := gitOps.CloneToDir(ctx, revision)
	if err != nil {
		return nil, fmt.Errorf("creating worktree at %s: %w", revision, err)
	}
	defer gitOps.RemoveWorktree(ctx, worktreePath)

	// Determine the cluster path within the worktree.
	relPath, err := filepath.Rel(repoRoot, clusterPath)
	if err != nil {
		relPath = clusterPath
	}
	worktreeClusterPath := filepath.Join(worktreePath, relPath)

	// Check if the path exists in the worktree.
	if _, err := os.Stat(worktreeClusterPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Warning: path %s does not exist at revision %s\n", relPath, revision)
		return nil, nil
	}

	// Check that the path contains Kustomization files directly (not just in subdirectories)
	hasDirectKS, err := hasDirectKustomizations(worktreeClusterPath)
	if err != nil {
		return nil, fmt.Errorf("checking for Kustomization files at %s: %w", revision, err)
	}
	if !hasDirectKS {
		return nil, fmt.Errorf("no Kustomization files found in %s at revision %s", worktreeClusterPath, revision)
	}

	parser := scans.parserFor(worktreeClusterPath)
	kustomizations, err := parser.ParseKustomizations(ctx)
	if err != nil {
		return nil, fmt.Errorf("parsing Kustomization resources at %s: %w", revision, err)
	}

	if name != "" {
		kustomizations = filterKustomizations(kustomizations, name)
		if len(kustomizations) == 0 {
			return nil, nil // KS doesn't exist at this revision — valid diff (added/removed)
		}
	}

	builder := kustomize.NewBuilder(worktreePath, env.ksCache.builderOptions()...)
	buildCache := make(buildCache)
	// Resolve ConfigMaps and Secrets for postBuild substitution from the worktree.
	configMaps := resolveConfigMaps(ctx, scans, worktreeClusterPath, builder, buildCache)
	secrets := resolveSecrets(ctx, scans, worktreeClusterPath, builder, buildCache)

	// Use worktreePath as repoRoot so that recursive discovery and postBuild
	// substitution work identically to the current state. External
	// GitRepository sources are fetched for both diff sides through the same
	// git source env (identity from the real repository, shared clone cache).
	return buildKSContent(ctx, &ksBuildEnv{
		scans:       scans,
		builder:     builder,
		repoRoot:    worktreePath,
		clusterPath: worktreeClusterPath,
		quiet:       true,
		cache:       buildCache,
		gitSources:  gitSources,
	}, kustomizations, substitutionSources{configMaps: configMaps, secrets: secrets})
}

// ksBuildEnv is the shared context of the Flux Kustomization pipeline:
// memoized scans, the kustomize builder, the roots the build resolves
// against, and the per-run knobs — quiet (suppress diagnostics), the build
// result cache, the anomaly report (nil = don't collect; build and diff
// stay lenient) and the external git source env (nil = strictly local).
type ksBuildEnv struct {
	scans       *scanCache
	builder     *kustomize.Builder
	repoRoot    string
	clusterPath string
	quiet       bool
	cache       buildCache
	report      *buildReport
	gitSources  *gitSourceEnv
}

// substitutionSources is the ConfigMap/Secret set Flux postBuild variable
// substitution resolves ${VAR} references against.
type substitutionSources struct {
	configMaps []flux.ConfigMap
	secrets    []flux.Secret
}

// buildKSContent is the shared build logic for Flux Kustomization resources,
// used by both build and diff commands. It runs buildAllKustomizations (which
// follows Flux controller behavior: recursive discovery, postBuild substitution,
// external GitRepository source fetching) and then appends native kustomize
// overlay outputs. env.gitSources enables building from external GitRepository
// upstreams; nil keeps everything strictly local.
func buildKSContent(ctx context.Context, env *ksBuildEnv, kustomizations []flux.Kustomization, subs substitutionSources) ([]byte, error) {
	output, err := buildAllKustomizations(ctx, env, kustomizations, subs)
	if err != nil {
		return nil, err
	}

	// Append native kustomize overlay outputs (vars/ etc.).
	// Skip overlays when no KS are selected (name filter returned empty).
	if len(kustomizations) > 0 {
		ksPaths := collectKustomizationPaths(env.repoRoot, kustomizations)
		overlayOutputs := buildKustomizeOverlays(ctx, env.scans, env.builder, env.clusterPath, ksPaths, env.cache)
		for _, overlay := range overlayOutputs {
			if len(output) > 0 {
				output = append(output, []byte(sectionSeparator)...)
			}
			output = append(output, reorderYAMLFields(overlay)...)
		}
	}

	// Deduplicate: overlay builds may reproduce resources already built
	// by buildAllKustomizations (e.g. when path resolution via securejoin
	// vs WalkDir produces different string representations on macOS symlinks).
	// Last occurrence wins (matches kustomize ResMap behavior).
	return yamlutil.DedupDocs(output), nil
}

// buildAllKustomizations runs kustomize build for all Kustomization resources,
// applies postBuild variable substitution from configMaps and secrets, and
// recursively discovers and builds new Kustomization resources found in the
// output (following Flux Kustomize controller behavior). A Kustomization
// whose path is missing locally but whose sourceRef names an external
// GitRepository (a repository other than the local origin) is fetched and
// built from the upstream clone instead — the mini source-controller.
func buildAllKustomizations(ctx context.Context, env *ksBuildEnv, kustomizations []flux.Kustomization, subs substitutionSources) ([]byte, error) {
	// Track already-processed KS by "namespace/name" to prevent duplicates.
	seen := make(map[string]bool)
	var results []string

	// Known GitRepository sources: parsed from the cluster path (with a
	// repoRoot fallback) plus everything recursively discovered in build
	// outputs — an external clone's output may reference further sources.
	knownRepos := collectKnownRepos(ctx, env)

	// Queue of KS to process.
	queue := make([]flux.Kustomization, len(kustomizations))
	copy(queue, kustomizations)

	maxDepth := 10 // Prevent infinite recursion

	for depth := 0; depth < maxDepth && len(queue) > 0; depth++ {
		var discoveredKS []flux.Kustomization

		for _, ks := range queue {
			key := fmt.Sprintf("%s/%s", ks.Metadata.Namespace, ks.Metadata.Name)
			if seen[key] || ks.Spec.Suspend {
				continue
			}
			seen[key] = true

			doc, discovered, err := buildOneKustomization(ctx, env, ks, key, knownRepos, seen, subs)
			if err != nil {
				return nil, err
			}
			results = append(results, doc)
			discoveredKS = append(discoveredKS, discovered...)
		}

		// Continue with newly discovered KS.
		queue = discoveredKS
	}

	if len(queue) > 0 {
		fmt.Fprintf(os.Stderr, "Warning: max recursion depth (%d) reached, %d Kustomization(s) not processed\n", maxDepth, len(queue))
	}

	if len(results) == 0 {
		return nil, nil
	}

	combined := strings.Join(results, sectionSeparator)

	// No dedup here on purpose: buildKSContent deduplicates the combined
	// output (KS builds + native overlays) once — deduplicating the KS part
	// alone would be pure extra work over a strict subset of that.
	return []byte(combined), nil
}

// collectKnownRepos parses the GitRepositories known before the first
// build: scanned from the cluster path with a repoRoot fallback. Only the
// external-source pipeline needs them; a nil gitSources keeps the map empty.
func collectKnownRepos(ctx context.Context, env *ksBuildEnv) map[string]flux.GitRepository {
	knownRepos := map[string]flux.GitRepository{}
	if env.gitSources == nil {
		return knownRepos
	}
	stderr := func(format string, args ...any) {
		if !env.quiet {
			fmt.Fprintf(os.Stderr, format, args...)
		}
	}
	for _, gr := range parseWithRootFallback(env.scans, env.clusterPath, env.repoRoot, "GitRepositories",
		func(p *flux.Parser) ([]flux.GitRepository, error) { return p.ParseGitRepositories(ctx) }, stderr) {
		knownRepos[gr.Metadata.Namespace+"/"+gr.Metadata.Name] = gr
	}
	return knownRepos
}

// encodeKustomization serializes the Flux Kustomization resource itself
// into the output (controller behavior). 2-space indent matches kustomize
// output formatting; an encode failure warns and yields nil.
func encodeKustomization(ks flux.Kustomization) []byte {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(ks); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to encode Kustomization %s/%s: %v\n",
			ks.Metadata.Namespace, ks.Metadata.Name, err)
	}
	enc.Close()
	return buf.Bytes()
}

// ksSourceResolution is where one Kustomization's content comes from:
// the resolved directory/file path, the root loose-file reads walk, and
// the external clone description when one was involved. fetchFailed marks
// a failed external fetch: the KS resource alone is still emitted, but
// without the missing-path warning (the fetch failure already warned).
type ksSourceResolution struct {
	sourcePath     string
	readRoot       string
	externalSource string
	fetchFailed    bool
}

// resolveKSSource resolves one Kustomization's source with Flux
// source-first semantics: spec.path always resolves against the
// repository named by sourceRef. An external GitRepository (other than
// the local origin) means the upstream clone — even when a directory of
// the same name happens to exist locally, it is a different repository's
// content. Everything else (local GitRepository, OCIRepository, Bucket,
// unknown source) resolves locally. A failed external fetch reports into
// env.report (validate fails on it) and yields an unusable resolution.
func resolveKSSource(ctx context.Context, env *ksBuildEnv, ks flux.Kustomization, key string, knownRepos map[string]flux.GitRepository) ksSourceResolution {
	res := ksSourceResolution{readRoot: env.repoRoot}
	if ks.Spec.Path == "" {
		return res
	}
	gr, ok := env.gitSources.lookupExternal(knownRepos, ks)
	if !ok {
		res.sourcePath = resolveSourcePath(env.repoRoot, ks)
		if _, err := os.Stat(res.sourcePath); os.IsNotExist(err) {
			res.sourcePath = ""
		}
		return res
	}

	res.externalSource = describeSource(ks)
	// The fetch is silent: for the user an external source must behave
	// exactly like a local one — the "Building ns/name" line is the only
	// progress output, identical to local Kustomizations. A failed fetch
	// warns under the same quiet contract as the rest of the diagnostics:
	// the hr pipeline's discovery stage is quiet (its output is
	// HelmReleases, the skip is symmetric), as is the diff comparison side.
	cloneDir, err := env.gitSources.ensure(ctx, gr)
	if err != nil {
		if !env.quiet {
			fmt.Fprintf(os.Stderr, "Warning: fetching %s for %s/%s failed: %v — skipping its resources\n",
				res.externalSource, ks.Metadata.Namespace, ks.Metadata.Name, err)
		}
		if env.report != nil {
			env.report.fetchErrors = append(env.report.fetchErrors, fetchError{
				ks:     key,
				source: res.externalSource,
				err:    err.Error(),
			})
		}
		res.fetchFailed = true
		return res
	}
	env.builder.AllowRoot(cloneDir)
	if resolved, err := securejoin.SecureJoin(cloneDir, ks.Spec.Path); err == nil {
		if _, statErr := os.Stat(resolved); statErr == nil {
			res.sourcePath = resolved
			// Loose-file reads under the clone are scoped to the clone
			// directory (the clone is the walk root's own "repository"),
			// not repoRoot.
			res.readRoot = cloneDir
		}
	}
	return res
}

// warnMissingKSSource reports a Kustomization whose source path is absent
// (in the external clone or locally) and records it for the validate
// gate — its resources would be silently missing from the checked set.
func warnMissingKSSource(ks flux.Kustomization, externalSource string, report *buildReport) {
	if ks.Spec.Path == "" {
		return
	}
	switch {
	case externalSource != "":
		fmt.Fprintf(os.Stderr, "Warning: %s/%s path %s not found in external source %s, skipping its resources\n",
			ks.Metadata.Namespace, ks.Metadata.Name, ks.Spec.Path, externalSource)
	case ks.Spec.SourceRef.Kind == flux.KindOCIRepository || ks.Spec.SourceRef.Kind == flux.KindBucket:
		fmt.Fprintf(os.Stderr, "Warning: %s/%s path %s not found locally (source kind %s is not fetched externally), skipping its resources\n",
			ks.Metadata.Namespace, ks.Metadata.Name, ks.Spec.Path, ks.Spec.SourceRef.Kind)
	default:
		fmt.Fprintf(os.Stderr, "Warning: %s/%s path %s not found locally, skipping its resources\n",
			ks.Metadata.Namespace, ks.Metadata.Name, ks.Spec.Path)
	}
	if report != nil {
		report.missingPaths = append(report.missingPaths, missingPath{
			ks:   fmt.Sprintf("%s/%s", ks.Metadata.Namespace, ks.Metadata.Name),
			path: ks.Spec.Path,
		})
	}
}

// buildOneKustomization builds one Kustomization and returns its output
// document, the Kustomizations recursively discovered in that output, and
// a context error. Unusable sources (failed fetch, missing path, failed
// build) degrade to the KS resource alone — exactly the controller's
// include-the-KS behavior — with the warning already printed. knownRepos
// is updated with the GitRepositories found in the built output.
func buildOneKustomization(ctx context.Context, env *ksBuildEnv, ks flux.Kustomization, key string, knownRepos map[string]flux.GitRepository, seen map[string]bool, subs substitutionSources) (string, []flux.Kustomization, error) {
	if err := CheckInterrupted(ctx); err != nil {
		return "", nil, err
	}
	ksYAML := encodeKustomization(ks)

	res := resolveKSSource(ctx, env, ks, key, knownRepos)
	if res.sourcePath == "" {
		if !res.fetchFailed {
			warnMissingKSSource(ks, res.externalSource, env.report)
		}
		return string(ksYAML), nil, nil
	}

	if !env.quiet {
		fmt.Fprintf(os.Stderr, "Building %s/%s\n", ks.Metadata.Namespace, ks.Metadata.Name)
	}

	output, err := renderKSContent(ctx, env, ks, res, subs)
	if err != nil {
		if !errors.Is(err, errAlreadyWarned) {
			fmt.Fprintf(os.Stderr, "Warning: build failed for %s/%s: %v\n",
				ks.Metadata.Namespace, ks.Metadata.Name, err)
		}
		return string(ksYAML), nil, nil
	}

	// Collect GitRepository sources referenced by the output — recursively
	// discovered Kustomizations may point at them.
	if env.gitSources != nil {
		for _, gr := range flux.ParseGitRepositoriesFromBytes(output) {
			knownRepos[gr.Metadata.Namespace+"/"+gr.Metadata.Name] = gr
		}
	}

	// Prepend the Kustomization resource to the build output.
	return prependKSResource(ksYAML, output), discoverResourcesFromOutput(output, seen), nil
}

// renderKSContent builds one Kustomization's source and applies the
// Flux post-build steps: spec.patches (JSON6902), spec.images and
// spec.targetNamespace first — one in-memory kustomize build for all
// three (the former ApplyPatches → ApplyImages → ApplyTargetNamespace
// chain cost three full parse → build → serialize cycles; on failure the
// untransformed output is kept, warn + continue) — then postBuild
// variable substitution LAST, as in real Flux right before apply, so
// ${VAR} references inside the content added by patches are resolved too.
func renderKSContent(ctx context.Context, env *ksBuildEnv, ks flux.Kustomization, res ksSourceResolution, subs substitutionSources) ([]byte, error) {
	output, err := buildSourcePath(ctx, env.scans, env.builder, res.sourcePath, res.readRoot, env.cache)
	if err != nil {
		return nil, err
	}

	if transformed, err := kustomize.ApplyTransformations(output, ks.Spec.Patches, ks.Spec.Images, ks.Spec.TargetNamespace, res.sourcePath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to apply transformations (patches/images/targetNamespace) for %s/%s: %v\n",
			ks.Metadata.Namespace, ks.Metadata.Name, err)
	} else {
		output = transformed
	}

	if flux.SubstituteNeeded(ks) {
		if vars := flux.ResolveSubstituteVars(ks, subs.configMaps, subs.secrets); len(vars) > 0 {
			output = flux.ApplySubstitution(output, vars)
		}
	}
	return output, nil
}

// prependKSResource prepends the Flux Kustomization resource to its own
// build output, with the plain "---" document separator (no surrounding
// blank lines: sectionSeparator adds those between KS results).
func prependKSResource(ksYAML, output []byte) string {
	if len(ksYAML) == 0 {
		return string(output)
	}
	combined := string(ksYAML)
	if len(output) > 0 {
		combined += "---\n" + string(output)
	}
	return combined
}

// discoverResourcesFromOutput parses build output for Flux Kustomization
// resources that haven't been seen yet.
func discoverResourcesFromOutput(data []byte, seen map[string]bool) []flux.Kustomization {
	var ksResults []flux.Kustomization

	for _, ks := range flux.ParseKustomizationsFromBytes(data) {
		key := fmt.Sprintf("%s/%s", ks.Metadata.Namespace, ks.Metadata.Name)
		if !seen[key] {
			ksResults = append(ksResults, ks)
		}
	}

	return ksResults
}

// buildSourcePath processes a Kustomization source path following the Flux
// Kustomize controller reconciliation logic:
//  1. If path is a file → read it directly as YAML resources
//  2. If path is a directory with kustomization.yaml → run kustomize build
//  3. If path is a directory without kustomization.yaml → discover and build
//     subdirectories that have their own kustomization.yaml (applies namespace
//     and other transformers), then read any remaining loose YAML files
//
// readRoot is the filesystem boundary for loose-file reads in case 3: the
// repository root for local paths, or the external source clone directory —
// os.Root scoping rejects symlinks resolving outside that boundary.
func buildSourcePath(ctx context.Context, scans *scanCache, builder *kustomize.Builder, sourcePath, readRoot string, cache buildCache) ([]byte, error) {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("source path %s: %w", sourcePath, err)
	}

	// Case 1: Path points to a single file — read directly.
	if !info.IsDir() {
		return os.ReadFile(sourcePath)
	}

	// Case 2: Directory with kustomization.yaml — run kustomize build.
	for _, name := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		if _, err := os.Stat(filepath.Join(sourcePath, name)); err == nil {
			if output, ok := buildDirCached(ctx, builder, sourcePath, cache); ok {
				return output, nil
			}
			return nil, errAlreadyWarned
		}
	}

	// Case 3: Directory without kustomization.yaml — discover subdirectories
	return buildSubdirectoriesAndLooseFiles(ctx, scans, builder, sourcePath, readRoot, cache)
}

// readYAMLFilesRecursive reads all .yaml/.yml files in a directory recursively
// and combines them into a single multi-document YAML output. Reads are scoped
// to repoRoot via os.Root so a symlink escaping the repository is rejected.
func readYAMLFilesRecursive(ctx context.Context, dir, repoRoot string) ([]byte, error) {
	root, err := os.OpenRoot(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("opening repo root %s: %w", repoRoot, err)
	}
	defer root.Close()

	var buf bytes.Buffer

	walkRoot := filepath.Clean(dir)
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if info.IsDir() {
			// Never cross a repository boundary: the .git directory itself
			// and nested git repository roots (external source clones cached
			// inside the working tree) are not fleet content.
			if info.Name() == ".git" || (filepath.Clean(path) != walkRoot && git.IsRepoRoot(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		data, err := fsx.ReadRootFile(root, repoRoot, path)
		if err != nil {
			return err
		}
		if buf.Len() > 0 {
			buf.WriteString(sectionSeparator)
		}
		buf.Write(data)
		return nil
	})

	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// inflateAllHelmReleases inflates all HelmRelease resources and returns combined YAML.
func inflateAllHelmReleases(ctx context.Context, inflater *helm.Inflater, input helmInflationInput, opts inflateOptions) ([]byte, error) {
	outputs, err := inflateHelmReleasesShared(ctx, inflater, input, opts)
	if err != nil {
		return nil, err
	}
	if len(outputs) == 0 {
		return nil, nil
	}

	var buf bytes.Buffer
	for i, out := range outputs {
		if err := CheckInterrupted(ctx); err != nil {
			return nil, err
		}
		if i > 0 {
			buf.WriteString(sectionSeparator)
		}
		buf.Write(out)
	}
	return buf.Bytes(), nil
}

// computeAndOutputDiff computes per-resource diffs and outputs them.
// Processing (redact, strip-attrs, skip-crds, resource split) is done in a
// single pass per state to avoid redundant YAML round-trips.
func computeAndOutputDiff(ctx context.Context, original, modified []byte, flags *DiffFlags) error {
	// Bail out before the first potentially heavy work (per-document parsing
	// of both states) if the command is already cancelled; a second
	// checkpoint follows after the parsing.
	if err := CheckInterrupted(ctx); err != nil {
		return err
	}

	// Namespace filtering happens inside buildResourceMap (metadata is
	// already parsed there) instead of a separate full re-parse per state.
	origMap := buildResourceMap(original, flags)
	modMap := buildResourceMap(modified, flags)

	// Deliberate: this fires on an empty final result, not only on an empty
	// namespace match — with --namespace X --skip-crds, a namespace holding
	// only CRDs reports this message instead of silently printing nothing.
	if flags.Namespace != "" && len(origMap) == 0 && len(modMap) == 0 {
		fmt.Fprintf(os.Stderr, "No resources found in namespace %q\n", flags.Namespace)
		return nil
	}

	// Check for interruption before expensive diff computation.
	if err := CheckInterrupted(ctx); err != nil {
		return err
	}

	diffs := diffResourceMaps(origMap, modMap, flags.Unified)

	if len(diffs) == 0 {
		fmt.Fprintf(os.Stderr, "No differences found.\n")
		return nil
	}

	colorMode := resolveColorMode(flags.Color)
	useColor := diffpkg.ShouldColor(colorMode)

	output := formatResourceDiffs(diffs, useColor)
	fmt.Print(output)

	return NewExitError(fmt.Errorf("differences found"), ExitDiffFound)
}
