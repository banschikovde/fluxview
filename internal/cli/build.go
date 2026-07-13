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

	clusterPath := flags.Path
	if clusterPath == "" {
		clusterPath = "."
	}

	absClusterPath, err := filepath.Abs(clusterPath)
	if err != nil {
		return NewExitError(fmt.Errorf("resolving path %s: %w", clusterPath, err), ExitCodeError)
	}

	if _, err := os.Stat(absClusterPath); os.IsNotExist(err) {
		return NewExitError(fmt.Errorf("path %s does not exist", clusterPath), ExitCodeError)
	}

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
	fmt.Fprintf(os.Stderr, "Building Kustomization resources from %s\n", clusterPath)

	hasDirectKS, err := hasDirectKustomizations(clusterPath)
	if err != nil {
		return NewExitError(fmt.Errorf("checking for Kustomization files: %w", err), ExitCodeError)
	}
	if !hasDirectKS {
		return NewExitError(fmt.Errorf("no Kustomization files found in %s", clusterPath), ExitCodeError)
	}

	parser := flux.NewParser(clusterPath)
	kustomizations, err := parser.ParseKustomizations(ctx)
	if err != nil {
		return NewExitError(fmt.Errorf("parsing Kustomization resources: %w", err), ExitCodeError)
	}

	builder := kustomize.NewBuilder(repoRoot)
	buildCache := make(buildCache)
	configMaps := resolveConfigMaps(ctx, clusterPath, builder, buildCache)

	kustomizations, err = flux.TopologicalSort(kustomizations)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v, processing in original order\n", err)
	}

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
		fmt.Fprintf(os.Stderr, "Building HelmRelease %s in %s\n", name, clusterPath)
	} else {
		fmt.Fprintf(os.Stderr, "Building all HelmReleases in %s\n", clusterPath)
	}

	hasDirectKS, err := hasDirectKustomizations(clusterPath)
	if err != nil {
		return NewExitError(fmt.Errorf("checking for Kustomization files: %w", err), ExitCodeError)
	}
	if !hasDirectKS {
		return NewExitError(fmt.Errorf("no Kustomization files found in %s", clusterPath), ExitCodeError)
	}

	output, err := buildHRInflation(ctx, clusterPath, repoRoot, name, flags.Namespace, false)
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}
	if len(bytes.TrimSpace(output)) == 0 {
		fmt.Fprintln(os.Stderr, "No HelmReleases found.")
		return nil
	}

	if flags.SkipCRDs {
		output = filterCRDDocs(output)
	}
	if flags.StripAttrs != "" {
		output = stripAllAttrs(output, flags.StripAttrs)
	}
	printResourcesBoxed(output)

	return nil
}

// --- Filters ---

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

// --- Build cache ---

// buildResult caches the outcome of a single kustomize build attempt —
// either the resulting bytes or the error. This prevents re-running and
// re-warning for the same directory across multiple code paths.
type buildResult struct {
	output []byte
	err    error
}

// buildCache maps directory path to its build result.
type buildCache map[string]buildResult

// buildDirCached runs builder.Build(dir) at most once per dir per cache
// lifetime. On failure it prints the warning exactly once and caches the
// error, so any later caller — across resolveConfigMaps, buildKustomizeOverlays,
// buildSubdirectoriesAndLooseFiles — silently skips instead of retrying.
func buildDirCached(builder *kustomize.Builder, dir string, cache buildCache) ([]byte, bool) {
	if res, seen := cache[dir]; seen {
		return res.output, res.err == nil
	}
	output, err := builder.Build(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: kustomize build %s failed: %v\n", dir, err)
	}
	cache[dir] = buildResult{output, err}
	return output, err == nil
}

// --- Path resolution ---

func resolveSourcePath(repoRoot string, ks flux.Kustomization) string {
	if ks.Spec.Path != "" {
		resolved, err := securejoin.SecureJoin(repoRoot, ks.Spec.Path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: cannot safely resolve path %s for %s/%s: %v, skipping\n",
				ks.Spec.Path, ks.Metadata.Namespace, ks.Metadata.Name, err)
			return ""
		}
		return resolved
	}
	return ""
}

func collectKustomizationPaths(repoRoot string, kustomizations []flux.Kustomization) map[string]bool {
	paths := make(map[string]bool)
	for _, ks := range kustomizations {
		if ks.Spec.Path != "" {
			resolved, err := securejoin.SecureJoin(repoRoot, ks.Spec.Path)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: cannot safely resolve path %s for %s/%s: %v, excluding from overlay builds\n",
					ks.Spec.Path, ks.Metadata.Namespace, ks.Metadata.Name, err)
				continue
			}
			paths[resolved] = true
		}
	}
	return paths
}

func buildKustomizeOverlays(clusterPath, repoRoot string, excludePaths map[string]bool, cache buildCache) [][]byte {
	kustomizeDirs, err := flux.DiscoverKustomizeDirs(clusterPath)
	builder := kustomize.NewBuilder(repoRoot)
	var outputs [][]byte

	// Track ALL directories that have a kustomization.yaml (attempted build),
	// not just successful ones. This prevents loose-file walker from reading
	// raw files out of a directory whose kustomization.yaml exists but build
	// failed — those files would leak as untransformed, plus the
	// kustomization.yaml itself would appear as a garbage document.
	kustDirs := make(map[string]bool)
	if err == nil {
		for _, dir := range kustomizeDirs {
			kustDirs[dir] = true
			if isExcludedDir(dir, excludePaths) {
				continue
			}
			if output, ok := buildDirCached(builder, dir, cache); ok {
				outputs = append(outputs, output)
			}
		}
	}
	// Also skip ANY directory containing a kustomization file (any kind),
	// including orphan kind: Component dirs not returned by DiscoverKustomizeDirs.
	// Their files are kustomize inputs, not standalone resources — the loose-file
	// walker must not emit them raw (neither the Component doc nor its resources).
	for _, dir := range mustDiscoverKustomizationFileDirs(clusterPath) {
		kustDirs[dir] = true
	}

	// Read loose YAML files from directories WITHOUT kustomization.yaml
	// that are not excluded and not inside any kustomize directory.
	// Only include files that look like k8s resources (have apiVersion + kind).
	filepath.Walk(clusterPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		dir := filepath.Dir(path)

		// Skip files inside any directory that has a kustomization.yaml
		// (regardless of build success/failure).
		for kd := range kustDirs {
			if dir == kd || strings.HasPrefix(dir, kd+string(filepath.Separator)) {
				return nil
			}
		}
		// Skip files inside excluded directories (spec.path of Flux Kustomizations).
		if isExcludedDir(dir, excludePaths) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		// Only include documents that look like k8s resources.
		filtered := filterK8sResources(data)
		if filtered != nil {
			outputs = append(outputs, filtered)
		}
		return nil
	})

	return outputs
}

// filterK8sResources returns only documents that look like k8s resources
// (have non-empty apiVersion and kind). Non-resource documents in a
// multi-doc YAML file are silently dropped.
func filterK8sResources(data []byte) []byte {
	var result []string
	for _, doc := range flux.SplitYAMLText(data) {
		var meta struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
		}
		if yaml.Unmarshal([]byte(doc), &meta) != nil || meta.APIVersion == "" || meta.Kind == "" {
			continue
		}
		result = append(result, doc)
	}
	if len(result) == 0 {
		return nil
	}
	return []byte(strings.Join(result, "\n---\n"))
}

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

// mustDiscoverKustomizationFileDirs returns directories under root that contain
// a kustomization file of any kind (Kustomization or Component). Errors are
// treated as "no dirs" — discovery failure must not abort the loose-file walker.
func mustDiscoverKustomizationFileDirs(root string) []string {
	dirs, _ := flux.DiscoverKustomizationFileDirs(root)
	return dirs
}

// --- ConfigMaps ---

func resolveConfigMaps(ctx context.Context, clusterPath string, builder *kustomize.Builder, cache buildCache) []flux.ConfigMap {
	parser := flux.NewParser(clusterPath)

	rawCMs, err := parser.ParseConfigMaps(ctx)
	if err != nil {
		rawCMs = nil
	}

	kustomizeDirs, err := flux.DiscoverKustomizeDirs(clusterPath)
	if err != nil {
		return rawCMs
	}

	var builtCMs []flux.ConfigMap
	for _, dir := range kustomizeDirs {
		output, ok := buildDirCached(builder, dir, cache)
		if !ok {
			continue
		}
		cms, err := flux.ParseConfigMapsFromBytes(output)
		if err != nil {
			continue
		}
		builtCMs = append(builtCMs, cms...)
	}

	return mergeSources(builtCMs, rawCMs, func(c flux.ConfigMap) string { return c.Metadata.Name })
}

// --- Utilities ---

func hasDirectKustomizations(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		if !flux.IsYAMLFile(entry.Name()) {
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
