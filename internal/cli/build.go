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
	"time"

	"github.com/cyphar/filepath-securejoin"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/git"
	"github.com/banschikovde/fluxview/internal/kustomize"
)

// BuildFlags holds flags for the build command.
type BuildFlags struct {
	Path                string
	Namespace           string
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
	registerHelmCacheFlags(cmd, &flags.HelmCacheDir, &flags.HelmIndexTTL, &flags.HelmDownloadTimeout)
	registerKustomizeCacheFlags(cmd, &flags.RemoteCacheDir, &flags.RemoteCacheTTL, &flags.RemoteCacheTimeout, &flags.BuildCacheDir, &flags.BuildCacheTTL, &flags.GitSourceCacheDir, &flags.GitSourceCacheTTL)

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
		return NewExitError(fmt.Errorf("unsupported resource type %q (use 'ks'/'kustomization' or 'hr'/'helmrelease')", resourceType), ExitCodeError)
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

	scans := newScanCache()
	parser := scans.parserFor(clusterPath)
	kustomizations, err := parser.ParseKustomizations(ctx)
	if err != nil {
		return NewExitError(fmt.Errorf("parsing Kustomization resources: %w", err), ExitCodeError)
	}

	ksCache := kustomizeCacheOptions{
		remoteDir:     flags.RemoteCacheDir,
		remoteTtl:     flags.RemoteCacheTTL,
		remoteTimeout: flags.RemoteCacheTimeout,
		buildCacheDir: flags.BuildCacheDir,
		buildCacheTTL: flags.BuildCacheTTL,
		gitSourceDir:  flags.GitSourceCacheDir,
		gitSourceTtl:  flags.GitSourceCacheTTL,
	}
	builder := kustomize.NewBuilder(repoRoot, ksCache.builderOptions()...)
	buildCache := make(buildCache)
	configMaps := resolveConfigMaps(ctx, scans, clusterPath, builder, buildCache)
	secrets := resolveSecrets(ctx, scans, clusterPath, builder, buildCache)

	kustomizations, err = flux.TopologicalSort(kustomizations)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v, processing in original order\n", err)
	}

	// Namespace filtering of the final output happens after the build
	// (filterByNamespace below); here only the name filter applies.
	if name != "" {
		kustomizations = filterKustomizations(kustomizations, name)
		if len(kustomizations) == 0 {
			return NewExitError(fmt.Errorf("kustomization %q not found", name), ExitCodeError)
		}
	}

	gitEnv := newGitSourceEnv(ctx, repoRoot, ksCache)
	defer gitEnv.Close()
	output, err := buildKSContent(ctx, scans, builder, kustomizations, repoRoot, clusterPath, configMaps, secrets, false, buildCache, nil, gitEnv)
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}

	if output != nil {
		// Single pass over the output documents: namespace filter, CRD
		// filter, attr stripping, reordering, conversion, redaction.
		entries := processResources(output, outputOptions{
			namespace:  flags.Namespace,
			skipCRDs:   flags.SkipCRDs,
			stripAttrs: parseAttrs(flags.StripAttrs),
		})
		if flags.Namespace != "" && len(entries) == 0 {
			// Deliberate: fires on an empty final result, not only an empty
			// namespace match (e.g. the namespace holds only CRDs and
			// --skip-crds dropped them).
			fmt.Fprintf(os.Stderr, "No resources found in namespace %q\n", flags.Namespace)
			return nil
		}
		printResourceEntries(entries)
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

	ksCache := kustomizeCacheOptions{
		remoteDir:     flags.RemoteCacheDir,
		remoteTtl:     flags.RemoteCacheTTL,
		remoteTimeout: flags.RemoteCacheTimeout,
		buildCacheDir: flags.BuildCacheDir,
		buildCacheTTL: flags.BuildCacheTTL,
		gitSourceDir:  flags.GitSourceCacheDir,
		gitSourceTtl:  flags.GitSourceCacheTTL,
	}
	gitEnv := newGitSourceEnv(ctx, repoRoot, ksCache)
	defer gitEnv.Close()
	output, err := buildHRInflation(ctx, newScanCache(), clusterPath, repoRoot, name, flags.Namespace, false, false,
		helmCacheOptions{dir: flags.HelmCacheDir, indexTTL: flags.HelmIndexTTL, downloadTimeout: flags.HelmDownloadTimeout},
		ksCache, gitEnv)
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}
	if len(bytes.TrimSpace(output)) == 0 {
		fmt.Fprintln(os.Stderr, "No HelmReleases found.")
		return nil
	}

	// CRD filtering is caller-side (--skip-crds): HR inflation itself never
	// drops CustomResourceDefinition documents from its output. The output is
	// processed (skip-crds / strip-attrs / reorder / convert / redact) in a
	// single pass over the documents.
	printResourceEntries(processResources(output, outputOptions{
		skipCRDs:   flags.SkipCRDs,
		stripAttrs: parseAttrs(flags.StripAttrs),
	}))

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

// buildReport collects non-fatal build anomalies that a strict validation
// gate treats as failures: Flux Kustomizations whose declared spec.path is
// absent from the local repository (or from the fetched external source
// clone), and external sources that could not be fetched — in both cases
// the resources are silently missing from the build output. A nil report
// means "don't collect" — build and diff stay lenient (warn only).
type buildReport struct {
	missingPaths []missingPath
	fetchErrors  []fetchError
}

// missingPath is one Flux Kustomization pointing at a path that could not
// be resolved locally.
type missingPath struct {
	ks   string // "namespace/name"
	path string // the declared spec.path
}

// fetchError is one Flux Kustomization whose external GitRepository source
// could not be fetched, so its resources are missing from the output.
type fetchError struct {
	ks     string // "namespace/name"
	source string // e.g. "GitRepository kyverno/kyverno"
	err    string
}

// buildDirCached runs builder.Build(dir) at most once per dir per cache
// lifetime. On failure it prints the warning exactly once and caches the
// error, so any later caller — across resolveConfigMaps, buildKustomizeOverlays,
// buildSubdirectoriesAndLooseFiles — silently skips instead of retrying.
func buildDirCached(ctx context.Context, builder *kustomize.Builder, dir string, cache buildCache) ([]byte, bool) {
	if res, seen := cache[dir]; seen {
		return res.output, res.err == nil
	}
	output, err := builder.Build(ctx, dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: kustomize build %s failed: %v\n", dir, err)
	}
	cache[dir] = buildResult{output, err}
	return output, err == nil
}

// failedDirs returns the sorted directories whose kustomize build failed.
// buildDirCached already printed the per-dir warnings with the underlying
// errors; this lists the failed paths so callers that must not proceed on
// a partial build (validate) can fail.
func (c buildCache) failedDirs() []string {
	var dirs []string
	for dir, res := range c {
		if res.err != nil {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs
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

// scopedRootReadFile reads absPath by opening it relative to root, so a symlink
// (or symlink chain) that resolves outside rootPath is rejected instead of
// followed. This closes the TOCTOU / symlink-escape gap (CWE-367) that a bare
// os.ReadFile has inside a filepath.Walk callback. root must have been created
// by os.OpenRoot(rootPath).
func scopedRootReadFile(root *os.Root, rootPath, absPath string) ([]byte, error) {
	rel, err := filepath.Rel(rootPath, absPath)
	if err != nil {
		return nil, err
	}
	f, err := root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
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

// --- ConfigMaps / Secrets ---

// resolveSourceResources merges two substitution-source sets of one resource
// kind: resources scanned directly (raw) from clusterPath and resources
// produced by kustomize builds under it (which may apply namespace
// transformation). Shared by resolveConfigMaps and resolveSecrets. Errors are
// non-fatal: a failed raw scan falls back to an empty set, and failed builds
// were already warned once by buildDirCached.
func resolveSourceResources[T any](ctx context.Context, scans *scanCache, clusterPath string, builder *kustomize.Builder, cache buildCache, parseRaw func(context.Context) ([]T, error), parseBuilt func([]byte) []T, nameOf func(T) string) []T {
	// Raw resources scanned directly from clusterPath.
	raw, _ := parseRaw(ctx)

	kustomizeDirs, _, err := scans.kustDirsAndFiles(ctx, clusterPath)
	if err != nil {
		return raw
	}

	var built []T
	for _, dir := range kustomizeDirs {
		output, ok := buildDirCached(ctx, builder, dir, cache)
		if !ok {
			continue
		}
		built = append(built, parseBuilt(output)...)
	}

	return mergeSources(built, raw, nameOf)
}

// resolveConfigMaps resolves ConfigMaps used for postBuild.substituteFrom /
// valuesFrom with kind: ConfigMap: raw ConfigMaps scanned directly from
// clusterPath are merged with ConfigMaps produced by kustomize builds.
func resolveConfigMaps(ctx context.Context, scans *scanCache, clusterPath string, builder *kustomize.Builder, cache buildCache) []flux.ConfigMap {
	return resolveSourceResources(ctx, scans, clusterPath, builder, cache,
		scans.parserFor(clusterPath).ParseConfigMaps,
		flux.ParseConfigMapsFromBytes,
		func(c flux.ConfigMap) string { return c.Metadata.Name },
	)
}

// resolveSecrets resolves Secrets used for postBuild.substituteFrom with
// kind: Secret. Mirrors resolveConfigMaps: raw Secrets scanned directly from
// clusterPath are merged with Secrets produced by kustomize builds (which may
// apply namespace transformation). Only key names matter for substitution —
// resolveSecrets never returns real secret values to the substitution path.
func resolveSecrets(ctx context.Context, scans *scanCache, clusterPath string, builder *kustomize.Builder, cache buildCache) []flux.Secret {
	return resolveSourceResources(ctx, scans, clusterPath, builder, cache,
		scans.parserFor(clusterPath).ParseSecrets,
		flux.ParseSecretsFromBytes,
		func(s flux.Secret) string { return s.Metadata.Name },
	)
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

		if flux.HasKustomizationsFromBytes(data) {
			return true, nil
		}
	}

	return false, nil
}
