package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cyphar/filepath-securejoin"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/git"
	"github.com/banschikovde/fluxview/internal/helm"
	"github.com/banschikovde/fluxview/internal/kustomize"
)

// BuildFlags holds flags for the build command.
type BuildFlags struct {
	Path       string
	Namespace  string
	SkipCRDs   bool
	StripAttrs string
}

func newBuildCmd() *cobra.Command {
	flags := &BuildFlags{}

	cmd := &cobra.Command{
		Use:   "build <resource> [name] [flags]",
		Short: "Build (assemble) Flux Kustomization or HelmRelease resources",
		Long: `Build Flux resources from a local git repository.

Resource types:
  ks, kustomization   — build all Kustomizations (kustomize + helm inflation)
  hr, helmrelease     — inflate HelmRelease chart(s)

If [name] is omitted, all resources of the type are processed.

Examples:
  fluxview build ks --path clusters/prod/flux/
  fluxview build ks --path clusters/prod/flux/ --skip-crds --strip-attrs status,creationTimestamp
  fluxview build hr --path clusters/prod/flux/
  fluxview build hr podinfo --path clusters/prod/flux/`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBuild(cmd.Context(), args, flags)
		},
	}

	cmd.Flags().StringVarP(&flags.Path, "path", "p", "", "Path to the cluster directory in the repository")
	cmd.Flags().StringVarP(&flags.Namespace, "namespace", "n", "", "Filter output resources by namespace")
	cmd.Flags().BoolVar(&flags.SkipCRDs, "skip-crds", false, "Skip CustomResourceDefinition resources in output")
	cmd.Flags().StringVar(&flags.StripAttrs, "strip-attrs", "", "Comma-separated keys to strip from output (e.g. helm.sh/chart,status)")

	return cmd
}

func runBuild(ctx context.Context, args []string, flags *BuildFlags) error {
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

	// Check that the path contains Kustomization files directly (not just in subdirectories)
	hasDirectKS, err := hasDirectKustomizations(clusterPath)
	if err != nil {
		return NewExitError(fmt.Errorf("checking for Kustomization files: %w", err), ExitCodeError)
	}
	if !hasDirectKS {
		return NewExitError(fmt.Errorf("no Kustomization files found in %s", clusterPath), ExitCodeError)
	}

	// Parse Flux Kustomization resources from the cluster path.
	parser := flux.NewParser(clusterPath)
	kustomizations, err := parser.ParseKustomizations(ctx)
	if err != nil {
		return NewExitError(fmt.Errorf("parsing Kustomization resources: %w", err), ExitCodeError)
	}

	// Build the kustomize resources via SDK (shared logic with diff command).
	builder := kustomize.NewBuilder()
	buildCache := make(map[string][]byte)

	// Resolve ConfigMaps for postBuild variable substitution.
	configMaps := resolveConfigMaps(ctx, clusterPath, builder, buildCache)

	// Sort Kustomizations by dependency order.
	kustomizations, err = flux.TopologicalSort(kustomizations)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v, processing in original order\n", err)
	}

	// Filter by name/namespace if specified.
	if name != "" || flags.Namespace != "" {
		kustomizations = filterKustomizations(kustomizations, name)
		if len(kustomizations) == 0 {
			return NewExitError(fmt.Errorf("Kustomization %q not found", name), ExitCodeError)
		}
	}

	output, err := buildKSContent(ctx, builder, kustomizations, repoRoot, clusterPath, configMaps, false, buildCache)
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}
	if output != nil {
		if flags.Namespace != "" {
			output = filterByNamespace(output, flags.Namespace)
			if len(output) == 0 {
				fmt.Fprintf(os.Stderr, "No resources found in namespace %q\n", flags.Namespace)
				return nil
			}
		}
		if flags.SkipCRDs {
			output = filterCRDDocs(output)
		}
		if flags.StripAttrs != "" {
			output = stripAllAttrs(output, flags.StripAttrs)
		}
		printRedacted(output)
	}

	// Inflate HelmReleases found in the cluster path.
	_, err = inflateHelmReleases(ctx, clusterPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: helm inflation failed: %v\n", err)
	}

	return nil
}

func runBuildHR(ctx context.Context, clusterPath, repoRoot, name string, flags *BuildFlags) error {
	if name != "" {
		fmt.Fprintf(os.Stderr, "Building HelmRelease %s in %s...\n", name, clusterPath)
	} else {
		fmt.Fprintf(os.Stderr, "Building all HelmReleases in %s...\n", clusterPath)
	}

	parser := flux.NewParser(clusterPath)

	// Resolve HelmReleases with namespace/name transformers applied by any
	// native kustomize overlay taken into account (see
	// resolveHelmReleasesWithKustomizeNamespaces for why this matters).
	builder := kustomize.NewBuilder()
	buildCache := make(map[string][]byte)
	helmReleases, err := resolveHelmReleasesWithKustomizeNamespaces(ctx, clusterPath, builder, buildCache)
	if err != nil {
		return NewExitError(fmt.Errorf("parsing HelmRelease resources: %w", err), ExitCodeError)
	}

	// Apply name filter if specified
	if name != "" {
		helmReleases = filterHelmReleases(helmReleases, name)
		if len(helmReleases) == 0 {
			return NewExitError(fmt.Errorf("HelmRelease %q not found", name), ExitCodeError)
		}
	}

	// Apply namespace filter if specified (filter HRs by namespace or target namespace)
	if flags.Namespace != "" {
		helmReleases = filterHelmReleasesByNamespace(helmReleases, flags.Namespace)
		if len(helmReleases) == 0 {
			return NewExitError(fmt.Errorf("no HelmReleases found in namespace %q", flags.Namespace), ExitCodeError)
		}
	}

	if len(helmReleases) == 0 {
		if name != "" {
			return NewExitError(fmt.Errorf("HelmRelease %q not found", name), ExitCodeError)
		}
		fmt.Fprintln(os.Stderr, "No HelmReleases found.")
		return nil
	}

	inflater, err := helm.NewInflater()
	if err != nil {
		return NewExitError(fmt.Errorf("initializing helm: %w", err), ExitCodeError)
	}

	// Parse resources needed for helm inflation
	helmRepos, err := parser.ParseHelmRepositories(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not parse HelmRepositories: %v\n", err)
	}

	ociRepos, err := parser.ParseOCIRepositories(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not parse OCIRepositories: %v\n", err)
	}

	configMaps, err := parser.ParseConfigMaps(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not parse ConfigMaps: %v\n", err)
	}

	secrets, err := parser.ParseSecrets(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not parse Secrets: %v\n", err)
	}

	for _, result := range inflateHelmReleasesShared(ctx, inflater, helmReleases, helmRepos, ociRepos, configMaps, secrets, flags.SkipCRDs) {
		printRedacted(result)
	}

	return nil
}

// printRedacted normalizes, reorders fields, strips SOPS metadata, and redacts
// secrets from the output, then prints to stdout.
// Returns the number of secrets redacted.
func printRedacted(data []byte) int {
	if data == nil {
		return 0
	}
	// Normalize: reorder fields (apiVersion, kind, metadata first) and strip SOPS.
	normalized := reorderYAMLFields(data)
	// Convert JSON-in-YAML to proper YAML (fixes namespace: null issues)
	converted, err := convertJSONInYAMLToYAML(normalized)
	if err != nil {
		// If conversion fails, use normalized
		converted = normalized
	}
	// Redact secret values.
	redacted := flux.RedactSecrets(converted)
	count := flux.CountSecrets(converted)
	fmt.Print(string(redacted))
	return count
}

// inflateHelmReleasesShared inflates all non-suspended HelmReleases and returns
// a slice of YAML outputs. Shared by build and diff commands.
func inflateHelmReleasesShared(ctx context.Context, inflater *helm.Inflater, helmReleases []flux.HelmRelease, helmRepos []flux.HelmRepository, ociRepos []flux.OCIRepository, configMaps []flux.ConfigMap, secrets []flux.Secret, skipCRDs bool) [][]byte {
	var outputs [][]byte
	for _, hr := range helmReleases {
		if err := CheckInterrupted(ctx); err != nil {
			return nil
		}

		if hr.Spec.Suspend {
			fmt.Fprintf(os.Stderr, "Skipping suspended HelmRelease %s/%s\n", hr.Metadata.Namespace, hr.Metadata.Name)
			continue
		}

		var repoURL string
		var username string
		var password string

		// ChartRef-based HR (Flux v2 OCIRepository pattern).
		if hr.Spec.ChartRef != nil && hr.Spec.ChartRef.Kind == flux.KindOCIRepository {
			ociRef, ociVersion := resolveOCIRepoURL(hr, ociRepos)
			if ociRef == "" {
				continue
			}
			// For OCIRepository, the full reference IS the chart name.
			hr.Spec.Chart.Spec.Chart = ociRef
			hr.Spec.Chart.Spec.Version = ociVersion
		} else {
			// Traditional chart.spec pattern.
			if hr.Spec.Chart.Spec.Chart == "" {
				continue
			}
			repoURL, username, password = resolveHelmRepoURL(hr, helmRepos, secrets)
			if repoURL == "" {
				continue
			}
		}

		fmt.Fprintf(os.Stderr, "Inflating HelmRelease %s/%s (chart: %s)...\n",
			hr.Metadata.Namespace, hr.Metadata.Name, hr.Spec.Chart.Spec.Chart)

		output, err := inflater.InflateHelmRelease(ctx, hr, repoURL, username, password, configMaps, secrets)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to inflate HelmRelease %s/%s: %v\n",
				hr.Metadata.Namespace, hr.Metadata.Name, err)
			continue
		}

		// Filter CRDs if requested
		if skipCRDs {
			output = filterCRDDocs(output)
		}

		outputs = append(outputs, output)
	}
	return outputs
}

// resolveOCIRepoURL finds the OCIRepository reference for a HelmRelease's chartRef.
// Returns (chartRef, version) where chartRef is the full OCI reference
// (URL, optionally with @digest appended), and version is semver/tag.
// For digest-based refs, chartRef includes @sha256:... and version is empty.
func resolveOCIRepoURL(hr flux.HelmRelease, ociRepos []flux.OCIRepository) (string, string) {
	if hr.Spec.ChartRef == nil {
		return "", ""
	}
	repoNS := hr.Spec.ChartRef.Namespace
	if repoNS == "" {
		repoNS = hr.Metadata.Namespace
	}
	for _, repo := range ociRepos {
		if repo.Metadata.Name == hr.Spec.ChartRef.Name && repo.Metadata.Namespace == repoNS {
			url := repo.Spec.URL
			ref := repo.Spec.Ref

			// Digest: highest priority — append @sha256:... to URL, no version.
			if ref.HasDigest() {
				return url + "@" + ref.Digest, ""
			}

			// Semver or tag → pass as version.
			return url, ref.ResolveVersion()
		}
	}
	return "", ""
}

// resolveHelmRepoURL finds the HelmRepository URL for a HelmRelease's chart.
func resolveHelmRepoURL(hr flux.HelmRelease, helmRepos []flux.HelmRepository, secrets []flux.Secret) (string, string, string) {
	sourceRef := hr.Spec.Chart.Spec.SourceRef
	if sourceRef.Kind != flux.KindHelmRepository {
		return "", "", ""
	}
	repoNS := sourceRef.Namespace
	if repoNS == "" {
		repoNS = hr.Metadata.Namespace
	}
	url, username, password, err := helm.FindHelmRepoURL(helmRepos, sourceRef.Name, repoNS, secrets)
	if err != nil || url == "" {
		return "", "", ""
	}
	return url, username, password
}

// inflateHelmReleases scans for HelmRelease resources in the cluster path and inflates them.
func inflateHelmReleases(ctx context.Context, clusterPath string) (int, error) {
	parser := flux.NewParser(clusterPath)
	helmReleases, err := parser.ParseHelmReleases(ctx)
	if err != nil {
		return 0, fmt.Errorf("parsing HelmReleases: %w", err)
	}

	if len(helmReleases) == 0 {
		return 0, nil
	}

	// Sort HelmReleases by dependency order.
	helmReleases, err = flux.TopologicalSortHelmReleases(helmReleases)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v, processing in original order\n", err)
	}

	helmRepos, err := parser.ParseHelmRepositories(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not parse HelmRepositories: %v\n", err)
	}

	ociRepos, err := parser.ParseOCIRepositories(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not parse OCIRepositories: %v\n", err)
	}

	configMaps, err := parser.ParseConfigMaps(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not parse ConfigMaps: %v\n", err)
	}

	secrets, err := parser.ParseSecrets(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not parse Secrets: %v\n", err)
	}

	inflater, err := helm.NewInflater()
	if err != nil {
		return 0, fmt.Errorf("initializing helm: %w", err)
	}

	var totalSecrets int
	for _, output := range inflateHelmReleasesShared(ctx, inflater, helmReleases, helmRepos, ociRepos, configMaps, secrets, false) {
		if err := CheckInterrupted(ctx); err != nil {
			return 0, nil
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

// filterHelmReleasesByNamespace filters HelmRelease resources by namespace.
// Includes HRs whose metadata.namespace matches OR whose targetNamespace matches.
func filterHelmReleasesByNamespace(resources []flux.HelmRelease, namespace string) []flux.HelmRelease {
	var result []flux.HelmRelease
	for _, hr := range resources {
		targetNamespace := hr.Spec.TargetNamespace
		if targetNamespace == "" {
			targetNamespace = hr.Metadata.Namespace
		}
		if hr.Metadata.Namespace == namespace || targetNamespace == namespace {
			result = append(result, hr)
		}
	}
	return result
}

// resolveSourcePath resolves the path for a Kustomization's source.
// Uses securejoin to prevent path traversal attacks - the resolved path
// is guaranteed to remain within the repoRoot directory.
// If securejoin fails, returns empty string to fail-closed and skip this Kustomization.
func resolveSourcePath(repoRoot string, ks flux.Kustomization) string {
	if ks.Spec.Path != "" {
		resolved, err := securejoin.SecureJoin(repoRoot, ks.Spec.Path)
		if err != nil {
			// Fail-closed: skip this Kustomization if we can't safely resolve its path
			// This prevents TOCTOU attacks via symlink loops that could trigger
			// the fallback to unsafe filepath.Join
			fmt.Fprintf(os.Stderr, "Warning: cannot safely resolve path %s for %s/%s: %v, skipping\n",
				ks.Spec.Path, ks.Metadata.Namespace, ks.Metadata.Name, err)
			return ""
		}
		return resolved
	}
	return ""
}

// collectKustomizationPaths collects absolute paths of all Flux Kustomization
// sources, used to exclude them from native kustomize overlay builds.
// Uses securejoin to prevent path traversal attacks.
// Paths that cannot be safely resolved are excluded from the map (fail-closed).
func collectKustomizationPaths(repoRoot string, kustomizations []flux.Kustomization) map[string]bool {
	paths := make(map[string]bool)
	for _, ks := range kustomizations {
		if ks.Spec.Path != "" {
			resolved, err := securejoin.SecureJoin(repoRoot, ks.Spec.Path)
			if err != nil {
				// Fail-closed: exclude paths that cannot be safely resolved
				// This prevents TOCTOU attacks via symlink loops
				fmt.Fprintf(os.Stderr, "Warning: cannot safely resolve path %s for %s/%s: %v, excluding from overlay builds\n",
					ks.Spec.Path, ks.Metadata.Namespace, ks.Metadata.Name, err)
				continue
			}
			paths[resolved] = true
		}
	}
	return paths
}

// buildKustomizeOverlays discovers and builds native kustomize overlays
// (e.g. vars/) under the cluster path. Returns the full build output
// for each overlay, containing ALL resources (ConfigMaps, Secrets, etc.).
// Excludes directories that are already built as Flux Kustomization sources.
func buildKustomizeOverlays(clusterPath string, excludePaths map[string]bool, buildCache map[string][]byte) [][]byte {
	kustomizeDirs, err := flux.DiscoverKustomizeDirs(clusterPath)
	if err != nil || len(kustomizeDirs) == 0 {
		return nil
	}

	builder := kustomize.NewBuilder()
	var outputs [][]byte

	for _, dir := range kustomizeDirs {
		// Skip directories already built as Flux Kustomization sources,
		// including subdirectories of KS source paths.
		if isExcludedDir(dir, excludePaths) {
			continue
		}
		var output []byte
		var err error

		// Check cache first
		if cached, ok := buildCache[dir]; ok {
			output = cached
		} else {
			output, err = builder.Build(dir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: kustomize build %s failed: %v\n", dir, err)
				continue
			}
			buildCache[dir] = output
		}
		outputs = append(outputs, output)
	}

	return outputs
}

// isExcludedDir returns true if dir is an excluded path or a subdirectory of one.
func isExcludedDir(dir string, excludePaths map[string]bool) bool {
	if excludePaths[dir] {
		return true
	}
	for ex := range excludePaths {
		if strings.HasPrefix(dir, ex+string(filepath.Separator)) {
			return true
		}
	}
	return false
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
		// Top-level key: no indentation, not a list item (-), not a comment (#),
		// and contains ":".
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' && line[0] != '-' && line[0] != '#' && strings.Contains(line, ":") {
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
//  1. Raw ConfigMap YAML files found directly in the cluster path
//  2. ConfigMaps produced by running kustomize build on native kustomize overlays
//     (e.g. vars/ directories that generate ConfigMaps via patches)
//
// The builder and buildCache are used to avoid redundant builds of the same directories.
// Errors are handled internally and logged as warnings, no error is returned.
func resolveConfigMaps(ctx context.Context, clusterPath string, builder *kustomize.Builder, buildCache map[string][]byte) []flux.ConfigMap {
	parser := flux.NewParser(clusterPath)

	// 1. Parse raw ConfigMap YAML files.
	configMaps, err := parser.ParseConfigMaps(ctx)
	if err != nil {
		configMaps = nil
	}

	// 2. Discover native kustomize directories and build them to find ConfigMaps.
	kustomizeDirs, err := flux.DiscoverKustomizeDirs(clusterPath)
	if err != nil {
		return configMaps
	}

	if len(kustomizeDirs) > 0 {
		for _, dir := range kustomizeDirs {
			var output []byte
			var err error

			// Check cache first
			if cached, ok := buildCache[dir]; ok {
				output = cached
			} else {
				output, err = builder.Build(dir)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Warning: kustomize build %s failed: %v\n", dir, err)
					continue
				}
				buildCache[dir] = output
			}

			builtCMs, err := flux.ParseConfigMapsFromBytes(output)
			if err != nil {
				continue
			}
			configMaps = append(configMaps, builtCMs...)
		}
	}

	return configMaps
}

// resolveHelmReleasesWithKustomizeNamespaces returns HelmRelease resources
// found under clusterPath, with namespace/name transformers applied by
// kustomize (e.g. a top-level `namespace:` field in kustomization.yaml)
// correctly reflected.
//
// Reading a HelmRelease YAML file directly (as flux.Parser.ParseHelmReleases
// does) only sees whatever `metadata.namespace` is literally written in that
// file. If the HelmRelease is actually managed by a native kustomize overlay
// that sets `namespace:` (or namePrefix/nameSuffix) for everything it builds,
// the file on disk may have no namespace at all, or a different one than what
// Flux will actually apply on the cluster — the transformer only takes effect
// when kustomize itself builds the resource.
//
// To account for this, HelmReleases are additionally extracted from the
// output of `kustomize build` for every native kustomize directory discovered
// under clusterPath (mirrors resolveConfigMaps' approach for ConfigMaps).
// A kustomize-resolved HelmRelease (identified by name) takes precedence over
// the raw-parsed one; HelmReleases not covered by any kustomize directory
// (loose files with no owning kustomization.yaml) fall back to the raw parse,
// since there is no transformer to apply for them anyway.
//
// Note: dedup is by name only. If a kustomize overlay also applies a
// namePrefix/nameSuffix transformer, the transformed name won't match the
// raw-parsed name, and both the raw and the kustomize-resolved HelmRelease
// may appear in the result. Fully correct name matching would require
// tracking resource identity through the kustomize build graph, which this
// function does not attempt.
func resolveHelmReleasesWithKustomizeNamespaces(ctx context.Context, clusterPath string, builder *kustomize.Builder, buildCache map[string][]byte) ([]flux.HelmRelease, error) {
	parser := flux.NewParser(clusterPath)

	// 1. Parse raw HelmRelease YAML files (namespace as literally written).
	rawHelmReleases, err := parser.ParseHelmReleases(ctx)
	if err != nil {
		return nil, err
	}

	// 2. Discover native kustomize directories and build them to resolve
	// namespace/name transformers.
	kustomizeDirs, dErr := flux.DiscoverKustomizeDirs(clusterPath)
	if dErr != nil {
		// Discovery failure isn't fatal — fall back to the raw parse.
		return rawHelmReleases, nil
	}

	var resolvedHelmReleases []flux.HelmRelease
	resolvedNames := make(map[string]bool)

	for _, dir := range kustomizeDirs {
		if err := CheckInterrupted(ctx); err != nil {
			return nil, err
		}

		var output []byte
		var buildErr error

		if cached, ok := buildCache[dir]; ok {
			output = cached
		} else {
			output, buildErr = builder.Build(dir)
			if buildErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: kustomize build %s failed: %v\n", dir, buildErr)
				continue
			}
			buildCache[dir] = output
		}

		builtHRs, parseErr := flux.ParseHelmReleasesFromBytes(output)
		if parseErr != nil {
			continue
		}
		for _, hr := range builtHRs {
			resolvedHelmReleases = append(resolvedHelmReleases, hr)
			resolvedNames[hr.Metadata.Name] = true
		}
	}

	// 3. Combine: kustomize-resolved HelmReleases (correct namespace) plus
	// any raw-parsed HelmRelease not already covered by a kustomize build.
	result := resolvedHelmReleases
	for _, hr := range rawHelmReleases {
		if !resolvedNames[hr.Metadata.Name] {
			result = append(result, hr)
		}
	}

	return result, nil
}

// hasDirectKustomizations checks if the given path contains Flux Kustomization
// resources directly (not in subdirectories).
func hasDirectKustomizations(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		if !isYAMLFile(entry.Name()) {
			continue
		}

		filePath := filepath.Join(path, entry.Name())
		data, err := os.ReadFile(filePath)
		if err != nil {
			continue
		}

		docs := flux.SplitYAMLDocuments(data)
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

			if meta.Kind == "Kustomization" && strings.HasPrefix(meta.APIVersion, "kustomize.toolkit.fluxcd.io") {
				return true, nil
			}
		}
	}

	return false, nil
}

func isYAMLFile(filename string) bool {
	ext := strings.ToLower(filepath.Ext(filename))
	return ext == ".yaml" || ext == ".yml"
}

// convertJSONInYAMLToYAML converts JSON-in-YAML format (e.g., metadata: {...})
// to proper YAML format (e.g., metadata:
//
//	annotations: ...).
//
// Helm v4 sometimes renders large objects like CRDs as JSON in YAML.
func convertJSONInYAMLToYAML(manifest []byte) ([]byte, error) {
	var result interface{}

	// Parse the manifest - yaml.Unmarshal handles both YAML and JSON
	if err := yaml.Unmarshal(manifest, &result); err != nil {
		return nil, err
	}

	// Marshal back to proper YAML format
	converted, err := yaml.Marshal(result)
	if err != nil {
		return nil, err
	}

	return converted, nil
}
