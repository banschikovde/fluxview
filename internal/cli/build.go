package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/banschikovde/flux-diff/internal/flux"
	"github.com/banschikovde/flux-diff/internal/git"
	"github.com/banschikovde/flux-diff/internal/helm"
	"github.com/banschikovde/flux-diff/internal/kustomize"
)

// BuildFlags holds flags for the build command.
type BuildFlags struct {
	Path string
}

func newBuildCmd() *cobra.Command {
	flags := &BuildFlags{}

	cmd := &cobra.Command{
		Use:   "build <resource> [name] [flags]",
		Short: "Build (assemble) Flux Kustomization or HelmRelease resources",
		Long: `Build Flux resources from a local git repository.

Supported resource types:
  ks — Flux Kustomization: runs kustomize build and inflates HelmReleases
  hr — Flux HelmRelease: inflates Helm chart via helm template

Examples:
  flux-diff build ks --path clusters/prod/
  flux-diff build hr podinfo --path clusters/prod/`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBuild(cmd.Context(), args, flags)
		},
	}

	cmd.Flags().StringVarP(&flags.Path, "path", "p", "", "Path to the cluster directory in the repository")

	return cmd
}

func runBuild(ctx context.Context, args []string, flags *BuildFlags) error {
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

	switch resourceType {
	case "ks", "kustomization":
		return runBuildKS(ctx, absClusterPath, repoRoot, name, flags)
	case "hr", "helmrelease":
		return runBuildHR(ctx, absClusterPath, repoRoot, name, flags)
	default:
		return NewExitError(fmt.Errorf("unsupported resource type %q (use 'ks' or 'hr')", resourceType), ExitCodeError)
	}
}

func runBuildKS(ctx context.Context, clusterPath, repoRoot, name string, flags *BuildFlags) error {
	fmt.Fprintf(os.Stderr, "Building Kustomization resources from %s...\n", clusterPath)

	// Parse Flux Kustomization resources from the cluster path.
	parser := flux.NewParser(clusterPath)
	kustomizations, err := parser.ParseKustomizations(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return nil
	}

	// Resolve ConfigMaps for postBuild variable substitution.
	configMaps, err := resolveConfigMaps(ctx, clusterPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not resolve ConfigMaps: %v\n", err)
	}

	// Sort Kustomizations by dependency order.
	kustomizations, err = flux.TopologicalSort(kustomizations)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v, processing in original order\n", err)
	}

	// Filter by name if specified.
	if name != "" {
		kustomizations = filterKustomizations(kustomizations, name)
		if len(kustomizations) == 0 {
			return NewExitError(fmt.Errorf("Kustomization %q not found", name), ExitCodeError)
		}
	}

	// Build the kustomize resources via SDK (shared with diff command).
	builder := kustomize.NewBuilder()
	output, err := buildAllKustomizations(builder, kustomizations, repoRoot, configMaps)
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}
	if output != nil {
		printRedacted(output)
	}

	// Build native kustomize overlays (e.g. vars/) and output ALL resources.
	ksPaths := collectKustomizationPaths(repoRoot, kustomizations)
	overlayOutputs := buildKustomizeOverlays(clusterPath, ksPaths)
	for _, out := range overlayOutputs {
		reordered := reorderYAMLFields(out)
		if !bytes.HasPrefix(reordered, []byte("---")) {
			fmt.Print("---\n")
		}
		printRedacted(reordered)
	}

	// Inflate HelmReleases found in the cluster path.
	_, err = inflateHelmReleases(ctx, clusterPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: helm inflation failed: %v\n", err)
	}

	return nil
}

func runBuildHR(ctx context.Context, clusterPath, repoRoot, name string, flags *BuildFlags) error {
	if name == "" {
		return NewExitError(fmt.Errorf("HelmRelease name is required for 'build hr' command"), ExitCodeError)
	}

	fmt.Fprintf(os.Stderr, "Building HelmRelease %s in %s...\n", name, clusterPath)

	// Parse HelmRelease resources.
	parser := flux.NewParser(clusterPath)
	helmReleases, err := parser.ParseHelmReleases(ctx)
	if err != nil {
		return NewExitError(fmt.Errorf("parsing HelmRelease resources: %w", err), ExitCodeError)
	}

	// Filter by name.
	helmReleases = filterHelmReleases(helmReleases, name)
	if len(helmReleases) == 0 {
		return NewExitError(fmt.Errorf("HelmRelease %q not found", name), ExitCodeError)
	}

	// Parse HelmRepository resources for resolving chart URLs.
	helmRepos, err := parser.ParseHelmRepositories(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not parse HelmRepositories: %v\n", err)
	}

	// Create helm inflater (Go SDK, no external binary needed).
	inflater, err := helm.NewInflater()
	if err != nil {
		return NewExitError(fmt.Errorf("initializing helm: %w", err), ExitCodeError)
	}

	for _, hr := range helmReleases {
		if hr.Spec.Suspend {
			fmt.Fprintf(os.Stderr, "Skipping suspended HelmRelease %s/%s\n", hr.Metadata.Namespace, hr.Metadata.Name)
			continue
		}

		// Skip partial HelmReleases (cluster-specific overlays without chart spec).
		if hr.Spec.Chart.Spec.Chart == "" {
			continue
		}

		// Find the HelmRepository URL.
		repoURL := ""
		sourceRef := hr.Spec.Chart.Spec.SourceRef
		if sourceRef.Kind == flux.KindHelmRepository {
			repoNS := sourceRef.Namespace
			if repoNS == "" {
				repoNS = hr.Metadata.Namespace
			}
			url, err := helm.FindHelmRepoURL(helmRepos, sourceRef.Name, repoNS)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
			} else {
				repoURL = url
			}
		}

		fmt.Fprintf(os.Stderr, "Inflating HelmRelease %s/%s (chart: %s)...\n",
			hr.Metadata.Namespace, hr.Metadata.Name, hr.Spec.Chart.Spec.Chart)

		output, err := inflater.InflateHelmRelease(ctx, hr, repoURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error inflating HelmRelease %s/%s: %v\n",
				hr.Metadata.Namespace, hr.Metadata.Name, err)
			continue
		}

		// Redact secrets and output to stdout.
		printRedacted(output)
	}

	return nil
}

// printRedacted redacts secrets from the output and prints to stdout.
// Returns the number of secrets redacted.
func printRedacted(data []byte) int {
	if data == nil {
		return 0
	}
	redacted := flux.RedactSecrets(data)
	count := flux.CountSecrets(data)
	fmt.Print(string(redacted))
	return count
}

// inflateHelmReleases scans for HelmRelease resources in the cluster path and inflates them.
// Returns the total number of secrets redacted and any error.
func inflateHelmReleases(ctx context.Context, clusterPath string) (int, error) {
	parser := flux.NewParser(clusterPath)
	helmReleases, err := parser.ParseHelmReleases(ctx)
	if err != nil {
		return 0, fmt.Errorf("parsing HelmReleases: %w", err)
	}

	if len(helmReleases) == 0 {
		return 0, nil
	}

	helmRepos, _ := parser.ParseHelmRepositories(ctx)

	inflater, err := helm.NewInflater()
	if err != nil {
		return 0, fmt.Errorf("initializing helm: %w", err)
	}

	var totalSecrets int
	for _, hr := range helmReleases {
		if hr.Spec.Suspend {
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

		totalSecrets += printRedacted(output)
	}

	return totalSecrets, nil
}

// filterKustomizations filters Kustomization resources by name.
func filterKustomizations(resources []flux.Kustomization, name string) []flux.Kustomization {
	var result []flux.Kustomization
	for _, ks := range resources {
		if name != "" && ks.Metadata.Name != name {
			continue
		}
		result = append(result, ks)
	}
	return result
}

// filterHelmReleases filters HelmRelease resources by name.
func filterHelmReleases(resources []flux.HelmRelease, name string) []flux.HelmRelease {
	var result []flux.HelmRelease
	for _, hr := range resources {
		if name != "" && hr.Metadata.Name != name {
			continue
		}
		result = append(result, hr)
	}
	return result
}

// resolveSourcePath resolves the path for a Kustomization's source.
func resolveSourcePath(repoRoot string, ks flux.Kustomization) string {
	if ks.Spec.Path != "" {
		return filepath.Join(repoRoot, ks.Spec.Path)
	}
	return ""
}

// collectKustomizationPaths collects absolute paths of all Flux Kustomization
// sources, used to exclude them from native kustomize overlay builds.
func collectKustomizationPaths(repoRoot string, kustomizations []flux.Kustomization) map[string]bool {
	paths := make(map[string]bool)
	for _, ks := range kustomizations {
		if ks.Spec.Path != "" {
			absPath := filepath.Join(repoRoot, ks.Spec.Path)
			paths[absPath] = true
		}
	}
	return paths
}

// buildKustomizeOverlays discovers and builds native kustomize overlays
// (e.g. vars/) under the cluster path. Returns the full build output
// for each overlay, containing ALL resources (ConfigMaps, Secrets, etc.).
// Excludes directories that are already built as Flux Kustomization sources.
func buildKustomizeOverlays(clusterPath string, excludePaths map[string]bool) [][]byte {
	kustomizeDirs, err := flux.DiscoverKustomizeDirs(clusterPath)
	if err != nil || len(kustomizeDirs) == 0 {
		return nil
	}

	builder := kustomize.NewBuilder()
	var outputs [][]byte

	for _, dir := range kustomizeDirs {
		// Skip directories already built as Flux Kustomization sources.
		if excludePaths[dir] {
			continue
		}
		output, err := builder.Build(dir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: kustomize build %s failed: %v\n", dir, err)
			continue
		}
		outputs = append(outputs, output)
	}

	return outputs
}

// reorderYAMLFields reorders top-level YAML fields to Kubernetes convention:
// apiVersion, kind, metadata first, then other fields in original order.
// Also strips SOPS metadata from Secret resources.
// This is a simple text-based reordering that doesn't parse YAML.
func reorderYAMLFields(data []byte) []byte {
	if len(data) == 0 {
		return data
	}

	// Split into YAML documents by lines that are exactly "---".
	var docs [][]byte
	var currentLines []string

	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "---" {
			if len(currentLines) > 0 {
				docs = append(docs, []byte(strings.Join(currentLines, "\n")))
				currentLines = nil
			}
		} else {
			currentLines = append(currentLines, line)
		}
	}
	if len(currentLines) > 0 {
		docs = append(docs, []byte(strings.Join(currentLines, "\n")))
	}

	var result [][]byte
	for _, doc := range docs {
		trimmed := bytes.TrimSpace(doc)
		if len(trimmed) == 0 {
			continue
		}
		// Strip SOPS metadata before reordering.
		trimmed = stripSOPSFields(trimmed)
		result = append(result, reorderSingleDoc(trimmed))
	}

	return bytes.Join(result, []byte("\n---\n"))
}

// stripSOPSFields removes the top-level "sops:" section from a YAML document.
// SOPS adds this metadata to encrypted files; it's not part of the Kubernetes resource.
func stripSOPSFields(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	var result []string
	skip := false

	for _, line := range lines {
		// Detect top-level "sops:" key (no indentation).
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' && strings.HasPrefix(line, "sops:") {
			skip = true
			continue
		}
		if skip {
			// Still inside the sops: block if the line is indented.
			if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
				continue
			}
			// Left the sops: block.
			skip = false
		}
		result = append(result, line)
	}

	return []byte(strings.Join(result, "\n"))
}

// reorderSingleDoc reorders top-level keys in a single YAML document.
func reorderSingleDoc(doc []byte) []byte {
	lines := strings.Split(string(doc), "\n")

	type section struct {
		key   string
		lines []string
	}
	var sections []section

	for _, line := range lines {
		// Top-level key: no indentation and contains ":"
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' && strings.Contains(line, ":") {
			key := strings.SplitN(line, ":", 2)[0]
			sections = append(sections, section{key: key, lines: []string{line}})
		} else if len(sections) > 0 {
			sections[len(sections)-1].lines = append(sections[len(sections)-1].lines, line)
		} else {
			// Lines before any section (e.g., comments) — add as separate section
			sections = append(sections, section{key: "", lines: []string{line}})
		}
	}

	// Priority order for top-level keys.
	priority := []string{"apiVersion", "kind", "metadata"}
	seen := make(map[string]bool)
	var ordered []section

	// Add priority sections first.
	for _, p := range priority {
		for _, sec := range sections {
			if sec.key == p && !seen[p] {
				ordered = append(ordered, sec)
				seen[p] = true
				break
			}
		}
	}

	// Add remaining sections in original order.
	for _, sec := range sections {
		if !seen[sec.key] {
			ordered = append(ordered, sec)
			seen[sec.key] = true
		}
	}

	// Rebuild document.
	var resultLines []string
	for _, sec := range ordered {
		resultLines = append(resultLines, sec.lines...)
	}

	return []byte(strings.Join(resultLines, "\n"))
}

// resolveConfigMaps discovers all ConfigMaps available for postBuild substitution.
// It combines:
// 1. Raw ConfigMap YAML files found directly in the cluster path
// 2. ConfigMaps produced by running kustomize build on native kustomize overlays
//    (e.g. vars/ directories that generate ConfigMaps via patches)
func resolveConfigMaps(ctx context.Context, clusterPath string) ([]flux.ConfigMap, error) {
	parser := flux.NewParser(clusterPath)

	// 1. Parse raw ConfigMap YAML files.
	configMaps, err := parser.ParseConfigMaps(ctx)
	if err != nil {
		configMaps = nil
	}

	// 2. Discover native kustomize directories and build them to find ConfigMaps.
	kustomizeDirs, err := flux.DiscoverKustomizeDirs(clusterPath)
	if err != nil {
		return configMaps, nil
	}

	if len(kustomizeDirs) > 0 {
		builder := kustomize.NewBuilder()
		for _, dir := range kustomizeDirs {
			output, err := builder.Build(dir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: kustomize build %s failed: %v\n", dir, err)
				continue
			}
			builtCMs, err := flux.ParseConfigMapsFromBytes(output)
			if err != nil {
				continue
			}
			configMaps = append(configMaps, builtCMs...)
		}
	}

	return configMaps, nil
}
