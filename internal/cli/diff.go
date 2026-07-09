package cli

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	diffpkg "github.com/banschikovde/fluxview/internal/diff"
	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/git"
	"github.com/banschikovde/fluxview/internal/helm"
	"github.com/banschikovde/fluxview/internal/kustomize"
	"gopkg.in/yaml.v3"
)

// DiffFlags holds flags for the diff command.
type DiffFlags struct {
	Path       string
	Namespace  string
	Color      string
	BranchOrig string
	Unified    int
	SkipCRDs   bool
	StripAttrs string
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
		return NewExitError(fmt.Errorf("unsupported resource type %q (use 'ks' or 'hr')", resourceType), ExitCodeError)
	}
}

// buildResult holds the output from building one diff state.
type buildResult struct {
	output []byte
	err    error
}

func runDiffKS(ctx context.Context, gitOps *git.Operations, clusterPath, repoRoot, name, compareCommit string, flags *DiffFlags) error {
	currentCh := make(chan buildResult, 1)
	compareCh := make(chan buildResult, 1)

	// Build current and revision states in parallel.
	go func() {
		output, err := buildKSOutput(ctx, clusterPath, repoRoot, name)
		currentCh <- buildResult{output, err}
	}()
	go func() {
		output, err := buildKSOutputAtRevision(ctx, gitOps, clusterPath, repoRoot, name, compareCommit)
		compareCh <- buildResult{output, err}
	}()

	current := <-currentCh
	if current.err != nil {
		return NewExitError(fmt.Errorf("building current state: %w", current.err), ExitCodeError)
	}

	compare := <-compareCh
	if compare.err != nil {
		return NewExitError(fmt.Errorf("building comparison state at %s: %w", compareCommit, compare.err), ExitCodeError)
	}

	return computeAndOutputDiff(compare.output, current.output, flags)
}

func runDiffHR(ctx context.Context, gitOps *git.Operations, clusterPath, repoRoot, name, compareCommit string, flags *DiffFlags) error {
	currentCh := make(chan buildResult, 1)
	compareCh := make(chan buildResult, 1)

	go func() {
		output, err := buildHROutput(ctx, clusterPath, name)
		currentCh <- buildResult{output, err}
	}()
	go func() {
		output, err := buildHROutputAtRevision(ctx, gitOps, clusterPath, repoRoot, name, compareCommit)
		compareCh <- buildResult{output, err}
	}()

	current := <-currentCh
	if current.err != nil {
		return NewExitError(fmt.Errorf("building current state: %w", current.err), ExitCodeError)
	}

	compare := <-compareCh
	if compare.err != nil {
		return NewExitError(fmt.Errorf("building comparison state at %s: %w", compareCommit, compare.err), ExitCodeError)
	}

	return computeAndOutputDiff(compare.output, current.output, flags)
}

// buildKSOutput builds the Kustomization output for the current working tree.
func buildKSOutput(ctx context.Context, clusterPath, repoRoot, name string) ([]byte, error) {
	parser := flux.NewParser(clusterPath)
	kustomizations, err := parser.ParseKustomizations(ctx)
	if err != nil {
		return nil, fmt.Errorf("parsing Kustomization resources: %w", err)
	}

	if name != "" {
		kustomizations = filterKustomizations(kustomizations, name)
		if len(kustomizations) == 0 {
			return nil, fmt.Errorf("Kustomization %q not found", name)
		}
	}

	// Resolve ConfigMaps for postBuild substitution.
	configMaps, _ := resolveConfigMaps(ctx, clusterPath)

	builder := kustomize.NewBuilder()
	return buildKSContent(ctx, builder, kustomizations, repoRoot, clusterPath, configMaps, false)
}

// buildKSOutputAtRevision builds the Kustomization output at a specific git revision.
func buildKSOutputAtRevision(ctx context.Context, gitOps *git.Operations, clusterPath, repoRoot, name, revision string) ([]byte, error) {
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
		if len(kustomizations) == 0 {
			return nil, nil // KS doesn't exist at this revision — valid diff (added/removed)
		}
	}

	// Resolve ConfigMaps for postBuild substitution from the worktree.
	configMaps, _ := resolveConfigMaps(ctx, worktreeClusterPath)

	builder := kustomize.NewBuilder()
	// Use worktreePath as repoRoot so that recursive discovery and postBuild
	// substitution work identically to the current state. External GitRepository
	// resolution is disabled for diff (too expensive — clones remote repos).
	return buildKSContent(ctx, builder, kustomizations, worktreePath, worktreeClusterPath, configMaps, true)
}

// buildHROutput builds the HelmRelease output for the current working tree.
func buildHROutput(ctx context.Context, clusterPath, name string) ([]byte, error) {
	parser := flux.NewParser(clusterPath)
	helmReleases, err := parser.ParseHelmReleases(ctx)
	if err != nil {
		return nil, fmt.Errorf("parsing HelmRelease resources: %w", err)
	}

	helmReleases = filterHelmReleases(helmReleases, name)
	if len(helmReleases) == 0 {
		if name != "" {
			return nil, fmt.Errorf("HelmRelease %q not found", name)
		}
		return nil, nil // no HRs found, return empty
	}

	helmRepos, _ := parser.ParseHelmRepositories(ctx)
	ociRepos, _ := parser.ParseOCIRepositories(ctx)

	inflater, err := helm.NewInflater()
	if err != nil {
		return nil, fmt.Errorf("initializing helm: %w", err)
	}

	return inflateAllHelmReleases(ctx, inflater, helmReleases, helmRepos, ociRepos)
}

// buildHROutputAtRevision builds the HelmRelease output at a specific git revision.
func buildHROutputAtRevision(ctx context.Context, gitOps *git.Operations, clusterPath, repoRoot, name, revision string) ([]byte, error) {
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
	if len(helmReleases) == 0 {
		return nil, nil // no HRs found
	}
	helmRepos, _ := parser.ParseHelmRepositories(ctx)
	ociRepos, _ := parser.ParseOCIRepositories(ctx)

	inflater, err := helm.NewInflater()
	if err != nil {
		return nil, fmt.Errorf("initializing helm: %w", err)
	}

	return inflateAllHelmReleases(ctx, inflater, helmReleases, helmRepos, ociRepos)
}

// buildKSContent is the shared build logic for Flux Kustomization resources,
// used by both build and diff commands. It runs buildAllKustomizations (which
// follows Flux controller behavior: recursive discovery, postBuild substitution,
// optional external GitRepository resolution) and then appends native kustomize
// overlay outputs.
func buildKSContent(ctx context.Context, builder *kustomize.Builder, kustomizations []flux.Kustomization, repoRoot, clusterPath string, configMaps []flux.ConfigMap, quiet bool) ([]byte, error) {
	output, err := buildAllKustomizations(ctx, builder, kustomizations, repoRoot, configMaps, quiet)
	if err != nil {
		return nil, err
	}

	// Append native kustomize overlay outputs (vars/ etc.).
	// Skip overlays when no KS are selected (name filter returned empty).
	if len(kustomizations) > 0 {
		ksPaths := collectKustomizationPaths(repoRoot, kustomizations)
		overlayOutputs := buildKustomizeOverlays(clusterPath, ksPaths)
		for _, overlay := range overlayOutputs {
			if len(output) > 0 {
				output = append(output, []byte("\n---\n")...)
			}
			output = append(output, reorderYAMLFields(overlay)...)
		}
	}

	return output, nil
}

// buildAllKustomizations runs kustomize build for all Kustomization resources,
// applies postBuild variable substitution from configMaps, and recursively
// discovers and builds new Kustomization resources found in the output
// (following Flux Kustomize controller behavior). When resolveExternal is true,
// Kustomizations referencing external GitRepositories are cloned and built;
// otherwise they are gracefully skipped.
func buildAllKustomizations(ctx context.Context, builder *kustomize.Builder, kustomizations []flux.Kustomization, repoRoot string, configMaps []flux.ConfigMap, quiet bool) ([]byte, error) {
	// Track already-processed KS by "namespace/name" to prevent duplicates.
	seen := make(map[string]bool)
	var results []string

	// Queue of KS to process.
	queue := make([]flux.Kustomization, len(kustomizations))
	copy(queue, kustomizations)

	maxDepth := 10 // Prevent infinite recursion

	for depth := 0; depth < maxDepth && len(queue) > 0; depth++ {
		var discoveredKS []flux.Kustomization

		for _, ks := range queue {
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
			ksEnc.Encode(ks)
			ksEnc.Close()
			ksYAML := ksYAMLBuf.Bytes()

			// Resolve source path from local repo.
			sourcePath := resolveSourcePath(repoRoot, ks)
			if sourcePath != "" {
				if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
					sourcePath = ""
				}
			}

			if sourcePath == "" {
				// Source not found locally — skip gracefully (KS resource only).
				if ksYAML != nil {
					results = append(results, string(ksYAML))
				}
				continue
			}

			if !quiet {
				fmt.Fprintf(os.Stderr, "Building %s/%s\n",
					ks.Metadata.Namespace, ks.Metadata.Name)
			}

			output, err := buildSourcePath(builder, sourcePath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: build failed for %s/%s: %v\n",
					ks.Metadata.Namespace, ks.Metadata.Name, err)
				if ksYAML != nil {
					results = append(results, string(ksYAML))
				}
				continue
			}

			// Apply postBuild variable substitution.
			if flux.SubstituteNeeded(ks) {
				vars := flux.ResolveSubstituteVars(ks, configMaps)
				if len(vars) > 0 {
					output = flux.ApplySubstitution(output, vars)
				}
			}

			// Scan output for new resources (KS only).
			newKS := discoverResourcesFromOutput(output, seen)
			if len(newKS) > 0 {
				discoveredKS = append(discoveredKS, newKS...)
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
		log.Printf("Warning: max recursion depth (%d) reached, %d Kustomization(s) not processed", maxDepth, len(queue))
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

// discoverResourcesFromOutput parses build output for Flux Kustomization
// resources that haven't been seen yet.
func discoverResourcesFromOutput(data []byte, seen map[string]bool) []flux.Kustomization {
	docs := flux.SplitYAMLDocuments(data)
	var ksResults []flux.Kustomization

	for _, doc := range docs {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" {
			continue
		}
		var meta struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
		}
		if err := yaml.Unmarshal([]byte(trimmed), &meta); err != nil {
			continue
		}

		// Discover Flux Kustomization resources.
		if meta.Kind == "Kustomization" && strings.HasPrefix(meta.APIVersion, "kustomize.toolkit.fluxcd.io") {
			var ks flux.Kustomization
			if err := yaml.Unmarshal([]byte(trimmed), &ks); err != nil {
				continue
			}
			key := fmt.Sprintf("%s/%s", ks.Metadata.Namespace, ks.Metadata.Name)
			if !seen[key] {
				ksResults = append(ksResults, ks)
			}
		}
	}

	return ksResults
}

// buildSourcePath processes a Kustomization source path following the Flux
// Kustomize controller reconciliation logic:
//  1. If path is a file → read it directly as YAML resources
//  2. If path is a directory with kustomization.yaml → run kustomize build
//  3. If path is a directory without kustomization.yaml → read all YAML files
func buildSourcePath(builder *kustomize.Builder, sourcePath string) ([]byte, error) {
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
			return builder.Build(sourcePath)
		}
	}

	// Case 3: Directory without kustomization.yaml — read all YAML files recursively.
	return readYAMLFilesRecursive(sourcePath)
}

// readYAMLFilesRecursive reads all .yaml/.yml files in a directory recursively
// and combines them into a single multi-document YAML output.
func readYAMLFilesRecursive(dir string) ([]byte, error) {
	var buf bytes.Buffer

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		data, err := os.ReadFile(path)
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
func inflateAllHelmReleases(ctx context.Context, inflater *helm.Inflater, helmReleases []flux.HelmRelease, helmRepos []flux.HelmRepository, ociRepos []flux.OCIRepository) ([]byte, error) {
	outputs := inflateHelmReleasesShared(ctx, inflater, helmReleases, helmRepos, ociRepos)
	if len(outputs) == 0 {
		return nil, nil
	}

	var buf bytes.Buffer
	for i, out := range outputs {
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
func computeAndOutputDiff(original, modified []byte, flags *DiffFlags) error {
	if flags.Namespace != "" {
		original = filterByNamespace(original, flags.Namespace)
		modified = filterByNamespace(modified, flags.Namespace)
		if len(original) == 0 && len(modified) == 0 {
			fmt.Fprintf(os.Stderr, "No resources found in namespace %q\n", flags.Namespace)
			return nil
		}
	}

	origMap := buildResourceMap(original, flags)
	modMap := buildResourceMap(modified, flags)

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
