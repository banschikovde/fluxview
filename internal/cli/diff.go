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
	RemoteCacheDir      string
	RemoteCacheTTL      time.Duration
	RemoteCacheTimeout  time.Duration
	BuildCacheDir       string
	BuildCacheTTL       time.Duration
	GitSourceCacheDir   string
	GitSourceCacheTTL   time.Duration
	// GitSourceSSHKnownHosts and GitSourceSSHAcceptNew are the external
	// git source auth policy knobs (credentials stay env-only).
	GitSourceSSHKnownHosts string
	GitSourceSSHAcceptNew  bool
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
	registerKustomizeCacheFlags(cmd, &flags.RemoteCacheDir, &flags.RemoteCacheTTL, &flags.RemoteCacheTimeout, &flags.BuildCacheDir, &flags.BuildCacheTTL, &flags.GitSourceCacheDir, &flags.GitSourceCacheTTL)
	registerGitSourceAuthFlags(cmd, &flags.GitSourceSSHKnownHosts, &flags.GitSourceSSHAcceptNew)
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
			return NewExitError(fmt.Errorf("could not determine default branch (use --branch): %w", err), ExitCodeError)
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
	scans := newScanCache()
	ksCache := kustomizeCacheOptions{
		remoteDir:     flags.RemoteCacheDir,
		remoteTtl:     flags.RemoteCacheTTL,
		remoteTimeout: flags.RemoteCacheTimeout,
		buildCacheDir: flags.BuildCacheDir,
		buildCacheTTL: flags.BuildCacheTTL,
		gitSourceDir:  flags.GitSourceCacheDir,
		gitSourceTtl:  flags.GitSourceCacheTTL,
	}
	// External source identity comes from the real repository (the git ops
	// handle), never from the comparison worktree — it has no remotes.
	gitEnv := newGitSourceEnv(ctx, repoRoot, ksCache)
	defer gitEnv.Close()

	currentOutput, err := buildKSOutput(ctx, scans, clusterPath, repoRoot, name, ksCache, gitEnv)
	if err != nil {
		return NewExitError(fmt.Errorf("building current state: %w", err), ExitCodeError)
	}

	compareOutput, err := buildKSOutputAtRevision(ctx, scans, gitOps, clusterPath, repoRoot, name, compareCommit, ksCache, gitEnv)
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
	gitEnv := newGitSourceEnv(ctx, repoRoot, kustomizeCacheOptions{
		remoteDir:     flags.RemoteCacheDir,
		remoteTtl:     flags.RemoteCacheTTL,
		remoteTimeout: flags.RemoteCacheTimeout,
		buildCacheDir: flags.BuildCacheDir,
		buildCacheTTL: flags.BuildCacheTTL,
		gitSourceDir:  flags.GitSourceCacheDir,
		gitSourceTtl:  flags.GitSourceCacheTTL,
	})
	defer gitEnv.Close()
	currentOutput, err := buildHRInflation(ctx, scans, clusterPath, repoRoot, name, flags.Namespace, false, true,
		helmCacheOptions{dir: flags.HelmCacheDir, indexTTL: flags.HelmIndexTTL, downloadTimeout: flags.HelmDownloadTimeout},
		kustomizeCacheOptions{
			remoteDir:     flags.RemoteCacheDir,
			remoteTtl:     flags.RemoteCacheTTL,
			remoteTimeout: flags.RemoteCacheTimeout,
			buildCacheDir: flags.BuildCacheDir,
			buildCacheTTL: flags.BuildCacheTTL,
			gitSourceDir:  flags.GitSourceCacheDir,
			gitSourceTtl:  flags.GitSourceCacheTTL,
		}, gitEnv)
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
		compareOutput, err := buildHRInflation(ctx, scans, worktreeClusterPath, worktreePath, name, flags.Namespace, true, true,
			helmCacheOptions{dir: flags.HelmCacheDir, indexTTL: flags.HelmIndexTTL, downloadTimeout: flags.HelmDownloadTimeout},
			kustomizeCacheOptions{
				remoteDir:     flags.RemoteCacheDir,
				remoteTtl:     flags.RemoteCacheTTL,
				remoteTimeout: flags.RemoteCacheTimeout,
				buildCacheDir: flags.BuildCacheDir,
				buildCacheTTL: flags.BuildCacheTTL,
				gitSourceDir:  flags.GitSourceCacheDir,
				gitSourceTtl:  flags.GitSourceCacheTTL,
			}, gitEnv)
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

// buildKSOutput builds the Kustomization output for the current working tree.
func buildKSOutput(ctx context.Context, scans *scanCache, clusterPath, repoRoot, name string, ksCache kustomizeCacheOptions, gitSources *gitSourceEnv) ([]byte, error) {
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

	builder := kustomize.NewBuilder(repoRoot, ksCache.builderOptions()...)
	buildCache := make(buildCache)
	// Resolve ConfigMaps and Secrets for postBuild substitution.
	configMaps := resolveConfigMaps(ctx, scans, clusterPath, builder, buildCache)
	secrets := resolveSecrets(ctx, scans, clusterPath, builder, buildCache)

	return buildKSContent(ctx, scans, builder, kustomizations, repoRoot, clusterPath, configMaps, secrets, false, buildCache, nil, gitSources)
}

// buildKSOutputAtRevision builds the Kustomization output at a specific git revision.
func buildKSOutputAtRevision(ctx context.Context, scans *scanCache, gitOps *git.Operations, clusterPath, repoRoot, name, revision string, ksCache kustomizeCacheOptions, gitSources *gitSourceEnv) ([]byte, error) {
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

	builder := kustomize.NewBuilder(worktreePath, ksCache.builderOptions()...)
	buildCache := make(buildCache)
	// Resolve ConfigMaps and Secrets for postBuild substitution from the worktree.
	configMaps := resolveConfigMaps(ctx, scans, worktreeClusterPath, builder, buildCache)
	secrets := resolveSecrets(ctx, scans, worktreeClusterPath, builder, buildCache)

	// Use worktreePath as repoRoot so that recursive discovery and postBuild
	// substitution work identically to the current state. External
	// GitRepository sources are fetched for both diff sides through the same
	// git source env (identity from the real repository, shared clone cache).
	return buildKSContent(ctx, scans, builder, kustomizations, worktreePath, worktreeClusterPath, configMaps, secrets, true, buildCache, nil, gitSources)
}

// buildKSContent is the shared build logic for Flux Kustomization resources,
// used by both build and diff commands. It runs buildAllKustomizations (which
// follows Flux controller behavior: recursive discovery, postBuild substitution,
// external GitRepository source fetching) and then appends native kustomize
// overlay outputs. gitSources enables building from external GitRepository
// upstreams; nil keeps everything strictly local.
func buildKSContent(ctx context.Context, scans *scanCache, builder *kustomize.Builder, kustomizations []flux.Kustomization, repoRoot, clusterPath string, configMaps []flux.ConfigMap, secrets []flux.Secret, quiet bool, cache buildCache, report *buildReport, gitSources *gitSourceEnv) ([]byte, error) {
	output, err := buildAllKustomizations(ctx, scans, builder, kustomizations, repoRoot, clusterPath, configMaps, secrets, quiet, cache, report, gitSources)
	if err != nil {
		return nil, err
	}

	// Append native kustomize overlay outputs (vars/ etc.).
	// Skip overlays when no KS are selected (name filter returned empty).
	if len(kustomizations) > 0 {
		ksPaths := collectKustomizationPaths(repoRoot, kustomizations)
		overlayOutputs := buildKustomizeOverlays(ctx, scans, builder, clusterPath, ksPaths, cache)
		for _, overlay := range overlayOutputs {
			if len(output) > 0 {
				output = append(output, []byte("\n---\n")...)
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
func buildAllKustomizations(ctx context.Context, scans *scanCache, builder *kustomize.Builder, kustomizations []flux.Kustomization, repoRoot, clusterPath string, configMaps []flux.ConfigMap, secrets []flux.Secret, quiet bool, cache buildCache, report *buildReport, gitSources *gitSourceEnv) ([]byte, error) {
	// Track already-processed KS by "namespace/name" to prevent duplicates.
	seen := make(map[string]bool)
	var results []string

	// Known GitRepository sources: parsed from the cluster path (with a
	// repoRoot fallback) plus everything recursively discovered in build
	// outputs — an external clone's output may reference further sources.
	knownRepos := map[string]flux.GitRepository{}
	if gitSources != nil {
		stderr := func(format string, args ...any) {
			if !quiet {
				fmt.Fprintf(os.Stderr, format, args...)
			}
		}
		for _, gr := range parseWithRootFallback(scans, clusterPath, repoRoot, "GitRepositories",
			func(p *flux.Parser) ([]flux.GitRepository, error) { return p.ParseGitRepositories(ctx) }, stderr) {
			knownRepos[gr.Metadata.Namespace+"/"+gr.Metadata.Name] = gr
		}
	}

	// Queue of KS to process.
	queue := make([]flux.Kustomization, len(kustomizations))
	copy(queue, kustomizations)

	maxDepth := 10 // Prevent infinite recursion

	for depth := 0; depth < maxDepth && len(queue) > 0; depth++ {
		var discoveredKS []flux.Kustomization

		for _, ks := range queue {
			if err := CheckInterrupted(ctx); err != nil {
				return nil, err
			}

			key := fmt.Sprintf("%s/%s", ks.Metadata.Namespace, ks.Metadata.Name)
			if seen[key] {
				continue
			}
			seen[key] = true

			if ks.Spec.Suspend {
				continue
			}

			// Include the Flux Kustomization resource itself (controller behavior).
			// Use 2-space indent to match kustomize output formatting.
			var ksYAMLBuf bytes.Buffer
			ksEnc := yaml.NewEncoder(&ksYAMLBuf)
			ksEnc.SetIndent(2)
			if err := ksEnc.Encode(ks); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to encode Kustomization %s/%s: %v\n",
					ks.Metadata.Namespace, ks.Metadata.Name, err)
			}
			ksEnc.Close()
			ksYAML := ksYAMLBuf.Bytes()

			// Resolve the source path with Flux source-first semantics:
			// spec.path always resolves against the repository named by
			// sourceRef. An external GitRepository (other than the local
			// origin) means the upstream clone — even when a directory of
			// the same name happens to exist locally, it is a different
			// repository's content. Everything else (local GitRepository,
			// OCIRepository, Bucket, unknown source) resolves locally.
			var sourcePath string
			readRoot := repoRoot
			externalSource := ""
			if ks.Spec.Path != "" {
				if gr, ok := gitSources.lookupExternal(knownRepos, ks); ok {
					externalSource = describeSource(ks)
					// The fetch is silent: for the user an external source
					// must behave exactly like a local one — the
					// "Building ns/name" line below is the only progress
					// output, identical to local Kustomizations.
					cloneDir, err := gitSources.ensure(ctx, gr)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Warning: fetching %s for %s/%s failed: %v — skipping its resources\n",
							externalSource, ks.Metadata.Namespace, ks.Metadata.Name, err)
						if report != nil {
							report.fetchErrors = append(report.fetchErrors, fetchError{
								ks:     key,
								source: externalSource,
								err:    err.Error(),
							})
						}
						if ksYAML != nil {
							results = append(results, string(ksYAML))
						}
						continue
					}
					builder.AllowRoot(cloneDir)
					if resolved, err := securejoin.SecureJoin(cloneDir, ks.Spec.Path); err == nil {
						if _, statErr := os.Stat(resolved); statErr == nil {
							sourcePath = resolved
							// Loose-file reads under the clone are scoped to
							// the clone directory (the clone is the walk
							// root's own "repository"), not repoRoot.
							readRoot = cloneDir
						}
					}
				} else {
					sourcePath = resolveSourcePath(repoRoot, ks)
					if sourcePath != "" {
						if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
							sourcePath = ""
						}
					}
				}
			}

			if sourcePath == "" {
				// Source not found — in the external clone or locally, per
				// the resolution above. Warn and include the KS resource
				// only. Validate additionally fails on this: its resources
				// would be silently absent from the checked set.
				if ks.Spec.Path != "" {
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
				if ksYAML != nil {
					results = append(results, string(ksYAML))
				}
				continue
			}

			if !quiet {
				fmt.Fprintf(os.Stderr, "Building %s/%s\n",
					ks.Metadata.Namespace, ks.Metadata.Name)
			}

			output, err := buildSourcePath(ctx, scans, builder, sourcePath, readRoot, cache)
			if err != nil {
				if !errors.Is(err, errAlreadyWarned) {
					fmt.Fprintf(os.Stderr, "Warning: build failed for %s/%s: %v\n",
						ks.Metadata.Namespace, ks.Metadata.Name, err)
				}
				if ksYAML != nil {
					results = append(results, string(ksYAML))
				}
				continue
			}

			// Apply Kustomization.spec.patches (JSON6902), spec.images,
			// and spec.targetNamespace — one in-memory kustomize build for
			// all three (the former ApplyPatches → ApplyImages →
			// ApplyTargetNamespace chain cost three full
			// parse → build → serialize cycles). On failure the
			// untransformed output is kept (warn + continue).
			transformed, err := kustomize.ApplyTransformations(output, ks.Spec.Patches, ks.Spec.Images, ks.Spec.TargetNamespace, sourcePath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to apply transformations (patches/images/targetNamespace) for %s/%s: %v\n",
					ks.Metadata.Namespace, ks.Metadata.Name, err)
			} else {
				output = transformed
			}

			// Apply postBuild variable substitution LAST, as in real Flux — right
			// before apply. This lets ${VAR} references inside the content added by
			// patches/images/targetNamespace be resolved too.
			if flux.SubstituteNeeded(ks) {
				vars := flux.ResolveSubstituteVars(ks, configMaps, secrets)
				if len(vars) > 0 {
					output = flux.ApplySubstitution(output, vars)
				}
			}

			// Scan output for new resources (KS only).
			newKS := discoverResourcesFromOutput(output, seen)
			if len(newKS) > 0 {
				discoveredKS = append(discoveredKS, newKS...)
			}

			// Collect GitRepository sources referenced by the output —
			// recursively discovered Kustomizations may point at them.
			if gitSources != nil {
				for _, gr := range flux.ParseGitRepositoriesFromBytes(output) {
					knownRepos[gr.Metadata.Namespace+"/"+gr.Metadata.Name] = gr
				}
			}

			// Prepend the Kustomization resource to the build output.
			if ksYAML != nil {
				combined := string(ksYAML)
				if len(output) > 0 {
					combined += "---\n" + string(output)
				}
				results = append(results, combined)
			} else {
				results = append(results, string(output))
			}
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

	combined := strings.Join(results, "\n---\n")

	// No dedup here on purpose: buildKSContent deduplicates the combined
	// output (KS builds + native overlays) once — deduplicating the KS part
	// alone would be pure extra work over a strict subset of that.
	return []byte(combined), nil
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
			buf.WriteString("\n---\n")
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
func inflateAllHelmReleases(ctx context.Context, inflater *helm.Inflater, helmReleases []flux.HelmRelease, helmRepos []flux.HelmRepository, ociRepos []flux.OCIRepository, configMaps []flux.ConfigMap, secrets []flux.Secret, opts inflateOptions) ([]byte, error) {
	outputs, err := inflateHelmReleasesShared(ctx, inflater, helmReleases, helmRepos, ociRepos, configMaps, secrets, opts)
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
			buf.WriteString("\n---\n")
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
