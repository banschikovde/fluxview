package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	diffpkg "github.com/banschikovde/flux-diff/internal/diff"
	"github.com/banschikovde/flux-diff/internal/flux"
	"github.com/banschikovde/flux-diff/internal/git"
	"github.com/banschikovde/flux-diff/internal/helm"
	"github.com/banschikovde/flux-diff/internal/kustomize"
)

// DiffFlags holds flags for the diff command.
type DiffFlags struct {
	Path   string
	Color  string
	Branch string
}

func newDiffCmd() *cobra.Command {
	flags := &DiffFlags{}

	cmd := &cobra.Command{
		Use:   "diff <resource> [name] [flags]",
		Short: "Compare Flux resources against another git revision",
		Long: `Compare Flux Kustomization or HelmRelease resources against another
git revision/branch and show the differences.

Supported resource types:
  ks — Flux Kustomization: diffs kustomize build output
  hr — Flux HelmRelease: diffs helm template output

Examples:
  flux-diff diff ks apps --path clusters/prod/
  flux-diff diff hr podinfo --path clusters/prod/
  flux-diff diff ks home --path clusters/dev/ --branch main`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiff(cmd.Context(), args, flags)
		},
	}

	cmd.Flags().StringVarP(&flags.Path, "path", "p", "", "Path to the cluster directory in the repository")
	cmd.Flags().StringVar(&flags.Color, "color", "auto", "Color mode: auto, always, never")
	cmd.Flags().StringVarP(&flags.Branch, "branch", "b", "", "Branch/revision to compare against (default: auto-detect default branch)")
	return cmd
}

func runDiff(ctx context.Context, args []string, flags *DiffFlags) error {
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

	compareRevision := flags.Branch
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
		return NewExitError(fmt.Errorf("unsupported resource type %q (use 'ks' or 'hr')", resourceType), ExitCodeError)
	}
}

func runDiffKS(ctx context.Context, gitOps *git.Operations, clusterPath, repoRoot, name, compareCommit string, flags *DiffFlags) error {
	fmt.Fprintf(os.Stderr, "Diffing Kustomization resources from %s...\n", clusterPath)

	// Build current state.
	currentOutput, err := buildKSOutput(clusterPath, repoRoot, name, flags)
	if err != nil {
		return NewExitError(fmt.Errorf("building current state: %w", err), ExitCodeError)
	}

	// Build comparison state (from the target revision).
	compareOutput, err := buildKSOutputAtRevision(ctx, gitOps, clusterPath, repoRoot, name, compareCommit, flags)
	if err != nil {
		return NewExitError(fmt.Errorf("building comparison state at %s: %w", compareCommit, err), ExitCodeError)
	}

	// Compute the diff.
	return computeAndOutputDiff(ctx, compareOutput, currentOutput, flags)
}

func runDiffHR(ctx context.Context, gitOps *git.Operations, clusterPath, repoRoot, name, compareCommit string, flags *DiffFlags) error {
	if name == "" {
		return NewExitError(fmt.Errorf("HelmRelease name is required for 'diff hr' command"), ExitCodeError)
	}

	fmt.Fprintf(os.Stderr, "Diffing HelmRelease %s from %s...\n", name, clusterPath)

	// Build current state.
	currentOutput, err := buildHROutput(ctx, clusterPath, repoRoot, name, flags)
	if err != nil {
		return NewExitError(fmt.Errorf("building current state: %w", err), ExitCodeError)
	}

	// Build comparison state (from the target revision).
	compareOutput, err := buildHROutputAtRevision(ctx, gitOps, clusterPath, repoRoot, name, compareCommit, flags)
	if err != nil {
		return NewExitError(fmt.Errorf("building comparison state at %s: %w", compareCommit, err), ExitCodeError)
	}

	// Compute the diff.
	return computeAndOutputDiff(ctx, compareOutput, currentOutput, flags)
}

// buildKSOutput builds the Kustomization output for the current working tree.
func buildKSOutput(clusterPath, repoRoot, name string, flags *DiffFlags) ([]byte, error) {
	parser := flux.NewParser(clusterPath)
	kustomizations, err := parser.ParseKustomizations(context.TODO())
	if err != nil {
		return nil, fmt.Errorf("parsing Kustomization resources: %w", err)
	}

	if name != "" {
		kustomizations = filterKustomizations(kustomizations, name)
	}

	// Resolve ConfigMaps for postBuild substitution.
	configMaps, _ := resolveConfigMaps(context.TODO(), clusterPath)

	builder := kustomize.NewBuilder()
	output, err := buildAllKustomizations(builder, kustomizations, repoRoot, configMaps)
	if err != nil {
		return nil, err
	}

	// Append native kustomize overlay outputs (vars/ etc.).
	ksPaths := collectKustomizationPaths(repoRoot, kustomizations)
	overlayOutputs := buildKustomizeOverlays(clusterPath, ksPaths)
	for _, overlay := range overlayOutputs {
		if len(output) > 0 {
			output = append(output, []byte("\n---\n")...)
		}
		output = append(output, reorderYAMLFields(overlay)...)
	}

	return output, nil
}

// buildKSOutputAtRevision builds the Kustomization output at a specific git revision.
func buildKSOutputAtRevision(ctx context.Context, gitOps *git.Operations, clusterPath, repoRoot, name, revision string, flags *DiffFlags) ([]byte, error) {
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

	parser := flux.NewParser(worktreeClusterPath)
	kustomizations, err := parser.ParseKustomizations(ctx)
	if err != nil {
		return nil, fmt.Errorf("parsing Kustomization resources at %s: %w", revision, err)
	}

	if name != "" {
		kustomizations = filterKustomizations(kustomizations, name)
	}

	builder := kustomize.NewBuilder()

	// Source paths need to be relative to the worktree, not the original repo.
	var results []string
	for _, ks := range kustomizations {
		if ks.Spec.Suspend {
			continue
		}
		sourcePath := filepath.Join(worktreePath, ks.Spec.Path)
		output, err := builder.Build(sourcePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: build failed for %s at %s: %v\n", ks.Metadata.Name, revision, err)
			continue
		}
		results = append(results, string(output))
	}

	if len(results) == 0 {
		return nil, nil
	}

	combined := ""
	for i, r := range results {
		if i > 0 {
			combined += "\n---\n"
		}
		combined += r
	}

	return []byte(combined), nil
}

// buildHROutput builds the HelmRelease output for the current working tree.
func buildHROutput(ctx context.Context, clusterPath, repoRoot, name string, flags *DiffFlags) ([]byte, error) {
	parser := flux.NewParser(clusterPath)
	helmReleases, err := parser.ParseHelmReleases(ctx)
	if err != nil {
		return nil, fmt.Errorf("parsing HelmRelease resources: %w", err)
	}

	helmReleases = filterHelmReleases(helmReleases, name)
	helmRepos, _ := parser.ParseHelmRepositories(ctx)

	inflater, err := helm.NewInflater()
	if err != nil {
		return nil, fmt.Errorf("initializing helm: %w", err)
	}

	return inflateAllHelmReleases(ctx, inflater, helmReleases, helmRepos)
}

// buildHROutputAtRevision builds the HelmRelease output at a specific git revision.
func buildHROutputAtRevision(ctx context.Context, gitOps *git.Operations, clusterPath, repoRoot, name, revision string, flags *DiffFlags) ([]byte, error) {
	// Create a git worktree at the target revision.
	worktreePath, err := gitOps.CloneToDir(ctx, revision)
	if err != nil {
		return nil, fmt.Errorf("creating worktree at %s: %w", revision, err)
	}
	defer gitOps.RemoveWorktree(ctx, worktreePath)

	relPath, err := filepath.Rel(repoRoot, clusterPath)
	if err != nil {
		relPath = clusterPath
	}
	worktreeClusterPath := filepath.Join(worktreePath, relPath)

	if _, err := os.Stat(worktreeClusterPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Warning: path %s does not exist at revision %s\n", relPath, revision)
		return nil, nil
	}

	parser := flux.NewParser(worktreeClusterPath)
	helmReleases, err := parser.ParseHelmReleases(ctx)
	if err != nil {
		return nil, fmt.Errorf("parsing HelmRelease resources at %s: %w", revision, err)
	}

	helmReleases = filterHelmReleases(helmReleases, name)
	helmRepos, _ := parser.ParseHelmRepositories(ctx)

	inflater, err := helm.NewInflater()
	if err != nil {
		return nil, fmt.Errorf("initializing helm: %w", err)
	}

	return inflateAllHelmReleases(ctx, inflater, helmReleases, helmRepos)
}

// buildAllKustomizations runs kustomize build for all Kustomization resources
// and applies postBuild variable substitution from configMaps.
func buildAllKustomizations(builder *kustomize.Builder, kustomizations []flux.Kustomization, repoRoot string, configMaps []flux.ConfigMap) ([]byte, error) {
	var results []string

	for _, ks := range kustomizations {
		if ks.Spec.Suspend {
			fmt.Fprintf(os.Stderr, "Skipping suspended Kustomization %s/%s\n", ks.Metadata.Namespace, ks.Metadata.Name)
			continue
		}

		sourcePath := resolveSourcePath(repoRoot, ks)
		if sourcePath == "" {
			fmt.Fprintf(os.Stderr, "Warning: could not resolve source path for Kustomization %s/%s, skipping\n",
				ks.Metadata.Namespace, ks.Metadata.Name)
			continue
		}

		fmt.Fprintf(os.Stderr, "Building Kustomization %s/%s (path: %s)...\n",
			ks.Metadata.Namespace, ks.Metadata.Name, sourcePath)

		output, err := builder.Build(sourcePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: build failed for %s/%s: %v\n",
				ks.Metadata.Namespace, ks.Metadata.Name, err)
			continue
		}

		// Apply postBuild variable substitution.
		if flux.SubstituteNeeded(ks) {
			vars := flux.ResolveSubstituteVars(ks, configMaps)
			if len(vars) > 0 {
				output = flux.ApplySubstitution(output, vars)
			}
		}

		results = append(results, string(output))
	}

	if len(results) == 0 {
		return nil, nil
	}

	combined := ""
	for i, r := range results {
		if i > 0 {
			combined += "\n---\n"
		}
		combined += r
	}

	return []byte(combined), nil
}

// inflateAllHelmReleases inflates all HelmRelease resources.
func inflateAllHelmReleases(ctx context.Context, inflater *helm.Inflater, helmReleases []flux.HelmRelease, helmRepos []flux.HelmRepository) ([]byte, error) {
	var results []string

	for _, hr := range helmReleases {
		if hr.Spec.Suspend {
			fmt.Fprintf(os.Stderr, "Skipping suspended HelmRelease %s/%s\n", hr.Metadata.Namespace, hr.Metadata.Name)
			continue
		}

		// Skip partial HelmReleases (cluster-specific overlays without chart spec).
		if hr.Spec.Chart.Spec.Chart == "" {
			continue
		}

		repoURL := ""
		sourceRef := hr.Spec.Chart.Spec.SourceRef
		if sourceRef.Kind == flux.KindHelmRepository {
			repoNS := sourceRef.Namespace
			if repoNS == "" {
				repoNS = hr.Metadata.Namespace
			}
			url, err := helm.FindHelmRepoURL(helmRepos, sourceRef.Name, repoNS)
			if err != nil || url == "" {
				continue
			}
			repoURL = url
		}

		if repoURL == "" {
			continue
		}

		output, err := inflater.InflateHelmRelease(ctx, hr, repoURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to inflate HelmRelease %s/%s: %v\n",
				hr.Metadata.Namespace, hr.Metadata.Name, err)
			continue
		}

		results = append(results, string(output))
	}

	if len(results) == 0 {
		return nil, nil
	}

	combined := ""
	for i, r := range results {
		if i > 0 {
			combined += "\n---\n"
		}
		combined += r
	}

	return []byte(combined), nil
}

// computeAndOutputDiff computes the diff and outputs it.
func computeAndOutputDiff(_ context.Context, original, modified []byte, flags *DiffFlags) error {
	// Redact secrets before comparison.
	if original != nil {
		original = flux.RedactSecrets(original)
	}
	if modified != nil {
		modified = flux.RedactSecrets(modified)
	}

	// Handle nil cases for comparison.
	origStr := ""
	modStr := ""
	if original != nil {
		origStr = string(original)
	}
	if modified != nil {
		modStr = string(modified)
	}

	// Use built-in diff.
	result := diffpkg.Compute(origStr, modStr)

	if !result.HasDiff {
		fmt.Fprintf(os.Stderr, "No differences found.\n")
		return nil
	}

	// Determine color mode.
	colorMode := resolveColorMode(flags.Color)
	useColor := diffpkg.ShouldColor(colorMode)

	// Format and output the diff.
	output := diffpkg.Format(result, useColor)
	fmt.Print(output)

	// Return special error to signal diff found (exit code 1).
	return NewExitError(fmt.Errorf("differences found"), ExitDiffFound)
}
