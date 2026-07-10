package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
  ks, kustomization   — build all Kustomizations (kustomize output only)
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
		printResourcesBoxed(output)
	}

	return nil
}

func runBuildHR(ctx context.Context, clusterPath, repoRoot, name string, flags *BuildFlags) error {
	if name != "" {
		fmt.Fprintf(os.Stderr, "Building HelmRelease %s in %s...\n", name, clusterPath)
	} else {
		fmt.Fprintf(os.Stderr, "Building all HelmReleases in %s...\n", clusterPath)
	}

	// Require Flux Kustomization resources in the path — same contract as build ks.
	hasDirectKS, err := hasDirectKustomizations(clusterPath)
	if err != nil {
		return NewExitError(fmt.Errorf("checking for Kustomization files: %w", err), ExitCodeError)
	}
	if !hasDirectKS {
		return NewExitError(fmt.Errorf("no Kustomization files found in %s", clusterPath), ExitCodeError)
	}

	kustomizations, err := flux.NewParser(clusterPath).ParseKustomizations(ctx)
	if err != nil {
		return NewExitError(fmt.Errorf("parsing Kustomization resources: %w", err), ExitCodeError)
	}

	builder := kustomize.NewBuilder()
	buildCache := make(map[string][]byte)
	configMaps := resolveConfigMaps(ctx, clusterPath, builder, buildCache)

	output, err := buildKSContent(ctx, builder, kustomizations, repoRoot, clusterPath, configMaps, true, buildCache)
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}

	// Extract HelmReleases from the build output — they have kustomize-transformed
	// namespaces and include HRs in shared bases pulled via spec.path.
	helmReleases, _ := flux.ParseHelmReleasesFromBytes(output)

	// Apply name filter if specified.
	if name != "" {
		helmReleases = filterHelmReleases(helmReleases, name)
		if len(helmReleases) == 0 {
			return NewExitError(fmt.Errorf("HelmRelease %q not found", name), ExitCodeError)
		}
	}

	// Apply namespace filter if specified.
	if flags.Namespace != "" {
		helmReleases = filterHelmReleasesByNamespace(helmReleases, flags.Namespace)
		if len(helmReleases) == 0 {
			return NewExitError(fmt.Errorf("no HelmReleases found in namespace %q", flags.Namespace), ExitCodeError)
		}
	}

	if len(helmReleases) == 0 {
		fmt.Fprintln(os.Stderr, "No HelmReleases found.")
		return nil
	}

	inflateAndPrintHelmReleases(ctx, helmReleases, clusterPath, repoRoot, output, flags.SkipCRDs)

	return nil
}

// resourceEntry holds a single YAML document with its resource key for sorting.
type resourceEntry struct {
	key     resourceKey
	content string
}

// printResourcesBoxed splits multi-doc YAML into individual resources, sorts
// them by kind/namespace/name, and prints each with a box header (same format
// as diff output). This replaces the flat YAML output of printRedacted.
func printResourcesBoxed(data []byte) {
	if len(bytes.TrimSpace(data)) == 0 {
		return
	}

	docs := flux.SplitYAMLText(data)
	var entries []resourceEntry

	for _, doc := range docs {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" {
			continue
		}

		var meta struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(trimmed), &meta); err != nil {
			continue
		}
		if meta.Kind == "" || meta.Metadata.Name == "" {
			continue
		}

		// Process: reorder fields, convert JSON-in-YAML, redact secrets.
		normalized := reorderYAMLFields([]byte(trimmed))
		converted, err := convertJSONInYAMLToYAML(normalized)
		if err != nil || converted == nil {
			continue
		}
		redacted := string(flux.RedactSecrets(converted))

		entries = append(entries, resourceEntry{
			key: resourceKey{
				Kind:      meta.Kind,
				Namespace: meta.Metadata.Namespace,
				Name:      meta.Metadata.Name,
			},
			content: strings.TrimSpace(redacted),
		})
	}

	// Sort by kind, namespace, name — same order as diff output.
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i].key, entries[j].key
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})

	for _, e := range entries {
		header := e.key.String()
		border := strings.Repeat("-", len(header)+2)
		fmt.Printf("%s\n %s\n%s\n%s\n\n", border, header, border, e.content)
	}
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
				fmt.Fprintf(os.Stderr, "Warning: could not resolve OCIRepository source for HelmRelease %s/%s (chartRef %s/%s) — not found, skipping\n",
					hr.Metadata.Namespace, hr.Metadata.Name, hr.Spec.ChartRef.Namespace, hr.Spec.ChartRef.Name)
				continue
			}
			// For OCIRepository, the full reference IS the chart name.
			hr.Spec.Chart.Spec.Chart = ociRef
			hr.Spec.Chart.Spec.Version = ociVersion
		} else {
			// Traditional chart.spec pattern.
			if hr.Spec.Chart.Spec.Chart == "" {
				fmt.Fprintf(os.Stderr, "Warning: HelmRelease %s/%s has no chart name, skipping\n",
					hr.Metadata.Namespace, hr.Metadata.Name)
				continue
			}
			repoURL, username, password = resolveHelmRepoURL(hr, helmRepos, secrets)
			if repoURL == "" {
				fmt.Fprintf(os.Stderr, "Warning: could not resolve source for HelmRelease %s/%s (chart %q) — HelmRepository not found, skipping\n",
					hr.Metadata.Namespace, hr.Metadata.Name, hr.Spec.Chart.Spec.Chart)
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

// inflateAndPrintHelmReleases resolves inflation sources (HelmRepository,
// OCIRepository, ConfigMap, Secret), inflates the given HelmReleases, and
// prints each output via printResourcesBoxed. Shared by runBuildHR.
// HelmReleases extracted from the kustomize build output) and runBuildHR.
//
// buildOutput is the kustomize build output from which the HelmReleases were
// extracted. Sources (HelmRepository, OCIRepository, ConfigMap) are also
// extracted from it and prepended to the raw-parsed sources — build-output
// sources have kustomize-transformed namespaces that match the HelmReleases.
func inflateAndPrintHelmReleases(ctx context.Context, helmReleases []flux.HelmRelease, clusterPath, repoRoot string, buildOutput []byte, skipCRDs bool) {
	// Deduplicate by namespace/name — two Flux Kustomizations may reference
	// the same shared base, causing the same HelmRelease to appear twice in
	// the build output.
	seen := make(map[string]bool)
	deduped := helmReleases[:0]
	for _, hr := range helmReleases {
		key := hr.Metadata.Namespace + "/" + hr.Metadata.Name
		if !seen[key] {
			seen[key] = true
			deduped = append(deduped, hr)
		}
	}
	helmReleases = deduped

	if len(helmReleases) == 0 {
		return
	}

	// Sort HelmReleases by dependency order.
	sorted, err := flux.TopologicalSortHelmReleases(helmReleases)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v, processing in original order\n", err)
		sorted = helmReleases
	}

	inflater, err := helm.NewInflater()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not initialize helm inflater: %v\n", err)
		return
	}

	// Extract sources from the build output first — these have kustomize-transformed
	// namespaces that match the HelmReleases (which also came from build output).
	// Prepend them so they are found before the raw-parsed versions.
	buildRepos, _ := flux.ParseHelmRepositoriesFromBytes(buildOutput)
	buildOCI, _ := flux.ParseOCIRepositoriesFromBytes(buildOutput)
	buildCMs, _ := flux.ParseConfigMapsFromBytes(buildOutput)

	rawRepos, rawOCI, rawCMs, secrets := resolveHelmInflationSources(ctx, clusterPath, repoRoot)

	helmRepos := append(buildRepos, rawRepos...)
	ociRepos := append(buildOCI, rawOCI...)
	configMaps := append(buildCMs, rawCMs...)

	for _, output := range inflateHelmReleasesShared(ctx, inflater, sorted, helmRepos, ociRepos, configMaps, secrets, skipCRDs) {
		if err := CheckInterrupted(ctx); err != nil {
			return
		}
		printResourcesBoxed(output)
	}
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

// resolveHelmInflationSources parses the source resources needed for HelmRelease
// inflation (HelmRepository, OCIRepository, ConfigMap, Secret). Each resource
// type is first parsed from clusterPath; if none are found there, the search
// falls back to repoRoot so that sources defined outside the cluster path
// (e.g. a shared sources/ or flux-system/ directory) are still resolved.
//
// Errors are logged as warnings and the corresponding slice may be nil — the
// caller (inflateHelmReleasesShared) handles missing sources gracefully by
// emitting a per-HelmRelease warning and skipping.
func resolveHelmInflationSources(ctx context.Context, clusterPath, repoRoot string) (helmRepos []flux.HelmRepository, ociRepos []flux.OCIRepository, configMaps []flux.ConfigMap, secrets []flux.Secret) {
	parser := flux.NewParser(clusterPath)

	helmRepos, err := parser.ParseHelmRepositories(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not parse HelmRepositories: %v\n", err)
		helmRepos = nil
	}
	if len(helmRepos) == 0 && repoRoot != "" && repoRoot != clusterPath {
		if rootRepos, rErr := flux.NewParser(repoRoot).ParseHelmRepositories(ctx); rErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not parse HelmRepositories from %s: %v\n", repoRoot, rErr)
		} else {
			helmRepos = rootRepos
		}
	}

	ociRepos, err = parser.ParseOCIRepositories(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not parse OCIRepositories: %v\n", err)
		ociRepos = nil
	}
	if len(ociRepos) == 0 && repoRoot != "" && repoRoot != clusterPath {
		if rootOCI, rErr := flux.NewParser(repoRoot).ParseOCIRepositories(ctx); rErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not parse OCIRepositories from %s: %v\n", repoRoot, rErr)
		} else {
			ociRepos = rootOCI
		}
	}

	configMaps, err = parser.ParseConfigMaps(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not parse ConfigMaps: %v\n", err)
		configMaps = nil
	}
	if len(configMaps) == 0 && repoRoot != "" && repoRoot != clusterPath {
		if rootCMs, rErr := flux.NewParser(repoRoot).ParseConfigMaps(ctx); rErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not parse ConfigMaps from %s: %v\n", repoRoot, rErr)
		} else {
			configMaps = rootCMs
		}
	}

	secrets, err = parser.ParseSecrets(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not parse Secrets: %v\n", err)
		secrets = nil
	}
	if len(secrets) == 0 && repoRoot != "" && repoRoot != clusterPath {
		if rootSecrets, rErr := flux.NewParser(repoRoot).ParseSecrets(ctx); rErr != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not parse Secrets from %s: %v\n", repoRoot, rErr)
		} else {
			secrets = rootSecrets
		}
	}

	return helmRepos, ociRepos, configMaps, secrets
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
// convertJSONInYAMLToYAML converts JSON-in-YAML format (e.g., metadata: {...})
// to proper YAML format (e.g., metadata:\n  annotations: ...).
// Helm v4 sometimes renders large objects like CRDs as JSON in YAML.
// Handles multi-document YAML by decoding each document separately.
// Removes nil map values (e.g. annotations: null) produced by Helm templates
// with empty optional fields.
func convertJSONInYAMLToYAML(manifest []byte) ([]byte, error) {
	var docs []string

	decoder := yaml.NewDecoder(bytes.NewReader(manifest))
	for {
		var doc interface{}
		if err := decoder.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			continue
		}
		if doc == nil {
			continue
		}
		doc = removeNilValues(doc)
		marshaled, err := yaml.Marshal(doc)
		if err != nil {
			continue
		}
		docs = append(docs, strings.TrimRight(string(marshaled), "\n"))
	}

	if len(docs) == 0 {
		return nil, nil
	}
	return []byte(strings.Join(docs, "\n---\n")), nil
}

// removeNilValues recursively removes map entries with nil values from the
// unmarshaled YAML data. This prevents annotations: null, labels: null, etc.
// in the output when Helm templates leave optional fields empty.
func removeNilValues(in interface{}) interface{} {
	switch v := in.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{})
		for k, val := range v {
			if val == nil {
				continue
			}
			result[k] = removeNilValues(val)
		}
		return result
	case []interface{}:
		for i, val := range v {
			v[i] = removeNilValues(val)
		}
		return v
	default:
		return in
	}
}
