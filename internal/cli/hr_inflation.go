package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/helm"
	"github.com/banschikovde/fluxview/internal/kustomize"
	"github.com/cyphar/filepath-securejoin"
)

// buildHRInflation discovers HelmReleases through the Flux Kustomization pipeline
// (same discovery logic as runBuildHR), resolves sources, inflates the charts,
// and returns combined YAML. Returns nil if no Flux Kustomizations or no
// HelmReleases are found (valid for diff comparison state).
//
// namespace filters the HelmRelease list BEFORE inflation — when set, only
// matching HRs are inflated, avoiding unnecessary chart downloads.
func buildHRInflation(ctx context.Context, clusterPath, repoRoot, name, namespace string, quiet bool) ([]byte, error) {
	kustomizations, err := flux.NewParser(clusterPath).ParseKustomizations(ctx)
	if err != nil {
		return nil, nil // no Flux KS — valid for diff
	}

	builder := kustomize.NewBuilder(repoRoot)
	buildCache := make(buildCache)
	configMaps := resolveConfigMaps(ctx, clusterPath, builder, buildCache)

	output, err := buildKSContent(ctx, builder, kustomizations, repoRoot, clusterPath, configMaps, true, buildCache)
	if err != nil {
		return nil, err
	}

	// Extract + dedup HelmReleases from the build output.
	allHRs, _ := flux.ParseHelmReleasesFromBytes(output)
	seen := make(map[string]bool)
	// In-place dedup reusing allHRs's backing array (allHRs[:0] aliases the
	// same storage). Safe because we only append values already read from
	// allHRs and never read past len(helmReleases); the source range loop
	// iterates the original allHRs by value.
	helmReleases := allHRs[:0]
	for _, hr := range allHRs {
		key := hr.Metadata.Namespace + "/" + hr.Metadata.Name
		if !seen[key] {
			seen[key] = true
			helmReleases = append(helmReleases, hr)
		}
	}

	// Name filter.
	if name != "" {
		helmReleases = filterHelmReleases(helmReleases, name)
		if len(helmReleases) == 0 {
			return nil, fmt.Errorf("HelmRelease %q not found", name)
		}
	}

	// Namespace filter — applied before inflation to avoid downloading unneeded charts.
	if namespace != "" {
		helmReleases = filterHelmReleasesByNamespace(helmReleases, namespace)
		if len(helmReleases) == 0 {
			return nil, nil
		}
	}

	if len(helmReleases) == 0 {
		return nil, nil
	}

	// Sort by dependency order.
	sorted, sortErr := flux.TopologicalSortHelmReleases(helmReleases)
	if sortErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v, processing in original order\n", sortErr)
		sorted = helmReleases
	}

	// Sources from build output (correct kustomize-transformed namespaces)
	// have priority over raw-parsed sources. Raw-parsed resources may have
	// stale literal namespaces from the source file that kustomize would
	// overwrite during build — including them as-is causes false exact-match
	// in ResolveValuesFrom when valuesFrom references the pre-transform namespace.
	buildRepos, _ := flux.ParseHelmRepositoriesFromBytes(output)
	buildOCI, _ := flux.ParseOCIRepositoriesFromBytes(output)
	buildCMs, _ := flux.ParseConfigMapsFromBytes(output)
	buildSecrets, _ := flux.ParseSecretsFromBytes(output)
	rawRepos, rawOCI, rawCMs, rawSecrets := resolveHelmInflationSources(ctx, clusterPath, repoRoot)

	// Merge: build-output versions are authoritative. Raw-parsed versions
	// only fill in resources NOT present in build output (by name).
	helmRepos := mergeSources(buildRepos, rawRepos, func(r flux.HelmRepository) string { return r.Metadata.Name })
	ociRepos := mergeSources(buildOCI, rawOCI, func(r flux.OCIRepository) string { return r.Metadata.Name })
	inflationCMs := mergeSources(buildCMs, rawCMs, func(c flux.ConfigMap) string { return c.Metadata.Name })
	secrets := mergeSources(buildSecrets, rawSecrets, func(s flux.Secret) string { return s.Metadata.Name })

	inflater, err := helm.NewInflater()
	if err != nil {
		return nil, fmt.Errorf("initializing helm: %w", err)
	}

	return inflateAllHelmReleases(ctx, inflater, sorted, helmRepos, ociRepos, inflationCMs, secrets, quiet, repoRoot)
}

// inflateHelmReleasesShared inflates all non-suspended HelmReleases and returns
// a slice of YAML outputs. Shared by build and diff commands.
func inflateHelmReleasesShared(ctx context.Context, inflater *helm.Inflater, helmReleases []flux.HelmRelease, helmRepos []flux.HelmRepository, ociRepos []flux.OCIRepository, configMaps []flux.ConfigMap, secrets []flux.Secret, skipCRDs bool, quiet bool, repoRoot string) [][]byte {
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
			hr.Spec.Chart.Spec.Chart = ociRef
			hr.Spec.Chart.Spec.Version = ociVersion
		} else {
			if hr.Spec.Chart.Spec.Chart == "" {
				fmt.Fprintf(os.Stderr, "Warning: HelmRelease %s/%s has no chart name, skipping\n",
					hr.Metadata.Namespace, hr.Metadata.Name)
				continue
			}
			// Chart sourced from a GitRepository: the chart already lives in the
			// local checkout, so resolve it as a directory path (no network).
			// Mirrors how Kustomization.spec.path is resolved via securejoin.
			// (Bucket sources are not supported — their content lives in object
			// storage, never in the git checkout; see README limitations.)
			if sourceKind := hr.Spec.Chart.Spec.SourceRef.Kind; sourceKind == flux.KindGitRepository {
				resolved, err := securejoin.SecureJoin(repoRoot, hr.Spec.Chart.Spec.Chart)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Warning: cannot safely resolve chart path %s for HelmRelease %s/%s: %v, skipping\n",
						hr.Spec.Chart.Spec.Chart, hr.Metadata.Namespace, hr.Metadata.Name, err)
					continue
				}
				info, err := os.Stat(resolved)
				if err != nil {
					fmt.Fprintf(os.Stderr, "Warning: chart %q for HelmRelease %s/%s not found locally (%s source) — skipping\n",
						hr.Spec.Chart.Spec.Chart, hr.Metadata.Namespace, hr.Metadata.Name, sourceKind)
					continue
				}
				// A chart source is either a directory (Chart.yaml inside) or a
				// packaged .tgz archive. Pointing at any other kind of file is
				// almost certainly a mistake — fail early with a clear message
				// rather than letting loader.Load emit a cryptic one.
				lower := strings.ToLower(resolved)
				isArchive := strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".tar.gz")
				if !info.IsDir() && !isArchive {
					fmt.Fprintf(os.Stderr, "Warning: chart path %q for HelmRelease %s/%s is not a chart directory or .tgz archive (%s source) — skipping\n",
						hr.Spec.Chart.Spec.Chart, hr.Metadata.Namespace, hr.Metadata.Name, sourceKind)
					continue
				}
				hr.Spec.Chart.Spec.Chart = resolved
				// repoURL stays empty → InflateHelmRelease renders from the local directory.
			} else if hr.Spec.Chart.Spec.SourceRef.Kind == flux.KindBucket {
				// Bucket content lives in object storage, never in the local git
				// checkout, so it cannot be resolved offline. Emit a dedicated,
				// explicit warning (see README limitations).
				fmt.Fprintf(os.Stderr, "Warning: Bucket-sourced chart for HelmRelease %s/%s (chart %q) is not supported, skipping\n",
					hr.Metadata.Namespace, hr.Metadata.Name, hr.Spec.Chart.Spec.Chart)
				continue
			} else {
				repoURL, username, password = resolveHelmRepoURL(hr, helmRepos, secrets)
				if repoURL == "" {
					fmt.Fprintf(os.Stderr, "Warning: could not resolve source for HelmRelease %s/%s (chart %q) — HelmRepository not found, skipping\n",
						hr.Metadata.Namespace, hr.Metadata.Name, hr.Spec.Chart.Spec.Chart)
					continue
				}
			}
		}

		if !quiet {
			fmt.Fprintf(os.Stderr, "Inflating HelmRelease %s/%s\n",
				hr.Metadata.Namespace, hr.Metadata.Name)
		}

		output, err := inflater.InflateHelmRelease(ctx, hr, repoURL, username, password, configMaps, secrets, repoRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to inflate HelmRelease %s/%s: %v\n",
				hr.Metadata.Namespace, hr.Metadata.Name, err)
			continue
		}

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

			if ref.HasDigest() {
				return url + "@" + ref.Digest, ""
			}

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

// resolveHelmInflationSources parses the source resources needed for HelmRelease
// inflation (HelmRepository, OCIRepository, ConfigMap, Secret). Each resource
// type is first parsed from clusterPath; if none are found there, the search
// falls back to repoRoot so that sources defined outside the cluster path
// (e.g. a shared sources/ or flux-system/ directory) are still resolved.
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

// mergeSources combines authoritative build-output sources with raw-parsed
// fallback sources. Resources from build are kept; raw resources are only
// added if no build-output resource with the same name exists. This prevents
// stale literal namespaces (pre-kustomize-transform) from causing false
// matches in ResolveValuesFrom.
//
// Trade-off: dedup is by name only, not name+namespace. If two legitimate
// resources with the same name exist in different namespaces (e.g. shared
// ConfigMap in team-a and team-b), and the team-a version is in build output,
// the team-b raw version is dropped. This is fail-safe (missing values + warning
// instead of wrong values), but could affect cross-namespace valuesFrom references.
func mergeSources[T any](build []T, raw []T, nameOf func(T) string) []T {
	seen := make(map[string]bool)
	result := make([]T, 0, len(build)+len(raw))

	for _, item := range build {
		seen[nameOf(item)] = true
		result = append(result, item)
	}
	for _, item := range raw {
		if !seen[nameOf(item)] {
			result = append(result, item)
		}
	}

	return result
}
