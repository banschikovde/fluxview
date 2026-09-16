package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/gitsource"
	"github.com/banschikovde/fluxview/internal/helm"
	"github.com/banschikovde/fluxview/internal/kustomize"
	"github.com/cyphar/filepath-securejoin"
)

// helmCacheOptions carries Helm cache settings from CLI flags/env into the
// Inflater: where the on-disk Helm cache lives, how long repository
// indexes stay fresh, and how long one HTTP download may take (0 = no
// limit). Independent of the kustomize cache.
type helmCacheOptions struct {
	dir             string
	indexTTL        time.Duration
	downloadTimeout time.Duration
}

func (o helmCacheOptions) inflaterOptions() []helm.InflaterOption {
	return []helm.InflaterOption{
		helm.WithCacheDir(o.dir),
		helm.WithIndexTTL(o.indexTTL),
		helm.WithDownloadTimeout(o.downloadTimeout),
	}
}

// kustomizeCacheOptions carries the kustomize caches from CLI flags/env
// into the kustomize Builder and the KS pipeline: the remote resource cache
// (remoteDir/remoteTtl), the build output cache
// (buildCacheDir/buildCacheTTL) and the external git source clone cache
// (gitSourceDir/gitSourceTtl, consumed by buildAllKustomizations — not a
// Builder option). Independent of the Helm cache. All follow the same
// conventions: dir "off"/"none"/"disabled" disables the cache, ttl 0
// always bypasses it (entries are still refreshed on disk).
type kustomizeCacheOptions struct {
	remoteDir     string
	remoteTtl     time.Duration
	remoteTimeout time.Duration
	buildCacheDir string
	buildCacheTTL time.Duration
	gitSourceDir  string
	gitSourceTtl  time.Duration
}

func (o kustomizeCacheOptions) builderOptions() []kustomize.BuilderOption {
	return []kustomize.BuilderOption{
		kustomize.WithRemoteCache(o.remoteDir, o.remoteTtl, o.remoteTimeout),
		kustomize.WithBuildCache(o.buildCacheDir, o.buildCacheTTL),
	}
}

// registerHelmCacheFlags registers the Helm cache flags shared by the
// commands. Defaults come from helm.DefaultCacheDir()/DefaultIndexTTL()/
// DefaultDownloadTimeout() so the FLUXVIEW_HELM_CACHE_DIR /
// FLUXVIEW_HELM_INDEX_TTL / FLUXVIEW_HELM_DOWNLOAD_TIMEOUT env vars are
// honored unless overridden by an explicit flag.
func registerHelmCacheFlags(cmd *cobra.Command, cacheDir *string, indexTTL, downloadTimeout *time.Duration) {
	cmd.Flags().StringVar(cacheDir, "helm-cache-dir", helm.DefaultCacheDir(),
		"Helm cache directory for repo indexes and downloaded charts (env: FLUXVIEW_HELM_CACHE_DIR)")
	cmd.Flags().DurationVar(indexTTL, "helm-index-ttl", helm.DefaultIndexTTL(),
		"How long cached Helm repo indexes and OCI tag resolutions stay fresh; 0 always refreshes (env: FLUXVIEW_HELM_INDEX_TTL)")
	cmd.Flags().DurationVar(downloadTimeout, "helm-download-timeout", helm.DefaultDownloadTimeout(),
		"Per-request timeout for downloading Helm repo indexes and chart tarballs; 0 = no limit, for slow networks (env: FLUXVIEW_HELM_DOWNLOAD_TIMEOUT)")
}

// registerKustomizeCacheFlags registers the kustomize cache flags shared by
// the commands, following one convention for every fluxview cache:
//
//	--<name>-cache-dir  cache directory; "off"/"none"/"disabled" disables
//	--<name>-cache-ttl  freshness window; 0 always bypasses (still writes)
//
// Here <name> is "remote" (resources fetched by URL, referenced by
// kustomizations), "build" (kustomize build outputs) and "git-source"
// (clones of external GitRepository sources). Defaults come from
// kustomize.DefaultRemoteCacheDir()/DefaultRemoteCacheTTL()/
// DefaultRemoteCacheTimeout()/DefaultBuildCacheDir()/DefaultBuildCacheTTL()/
// gitsource.DefaultCacheDir()/DefaultTTL().
func registerKustomizeCacheFlags(cmd *cobra.Command, remoteDir *string, remoteTtl, remoteTimeout *time.Duration, buildCacheDir *string, buildCacheTTL *time.Duration, gitSourceDir *string, gitSourceTtl *time.Duration) {
	cmd.Flags().StringVar(remoteDir, "remote-cache-dir", kustomize.DefaultRemoteCacheDir(),
		"Cache directory for remote resources referenced by kustomizations; off/none disables")
	cmd.Flags().DurationVar(remoteTtl, "remote-cache-ttl", kustomize.DefaultRemoteCacheTTL(),
		"How long cached remote resources with floating refs (branch/HEAD URLs) stay fresh; pinned version URLs never expire; 0 always re-fetches")
	cmd.Flags().DurationVar(remoteTimeout, "remote-cache-timeout", kustomize.DefaultRemoteCacheTimeout(),
		"Per-request timeout for downloading remote resources referenced by kustomizations; 0 = no limit, for slow networks (env: FLUXVIEW_REMOTE_CACHE_TIMEOUT)")
	cmd.Flags().StringVar(buildCacheDir, "build-cache-dir", kustomize.DefaultBuildCacheDir(),
		"Cache directory for kustomize build outputs, reused while input files are unchanged; off/none disables (env: FLUXVIEW_BUILD_CACHE_DIR)")
	cmd.Flags().DurationVar(buildCacheTTL, "build-cache-ttl", kustomize.DefaultBuildCacheTTL(),
		"How long cached kustomize build outputs stay usable; 0 always rebuilds (entries are still refreshed) (env: FLUXVIEW_BUILD_CACHE_TTL)")
	cmd.Flags().StringVar(gitSourceDir, "git-source-cache-dir", gitsource.DefaultCacheDir(),
		"Cache directory for clones of external GitRepository sources; off/none disables reuse (env: FLUXVIEW_GIT_SOURCE_CACHE_DIR)")
	cmd.Flags().DurationVar(gitSourceTtl, "git-source-cache-ttl", gitsource.DefaultTTL(),
		"How long floating external source resolutions (branch/semver/HEAD) stay fresh; pinned commit/tag clones never expire; 0 always re-resolves (env: FLUXVIEW_GIT_SOURCE_CACHE_TTL)")
}

// buildHRInflation discovers HelmReleases through the Flux Kustomization pipeline
// (same discovery logic as runBuildHR), resolves sources, inflates the charts,
// and returns combined YAML. Returns nil if no Flux Kustomizations or no
// HelmReleases are found (valid for diff comparison state).
//
// namespace filters the HelmRelease list BEFORE inflation — when set, only
// matching HRs are inflated, avoiding unnecessary chart downloads.
//
// strict (diff mode) turns skip-worthy inflation failures into a returned
// error instead of a warning + skip, so an unbuildable state never produces a
// misleading partial diff.
//
// gitSources enables external GitRepository source fetching for the
// Kustomization pipeline stage (nil keeps it local-only).
func buildHRInflation(ctx context.Context, scans *scanCache, clusterPath, repoRoot, name, namespace string, quiet, strict bool, helmCache helmCacheOptions, ksCache kustomizeCacheOptions, gitSources *gitSourceEnv) ([]byte, error) {
	kustomizations, err := scans.parserFor(clusterPath).ParseKustomizations(ctx)
	if err != nil {
		return nil, nil // no Flux KS — valid for diff
	}

	builder := kustomize.NewBuilder(repoRoot, ksCache.builderOptions()...)
	buildCache := make(buildCache)
	configMaps := resolveConfigMaps(ctx, scans, clusterPath, builder, buildCache)
	secrets := resolveSecrets(ctx, scans, clusterPath, builder, buildCache)

	output, err := buildKSContent(ctx, scans, builder, kustomizations, repoRoot, clusterPath, configMaps, secrets, true, buildCache, nil, gitSources)
	if err != nil {
		return nil, err
	}

	// One pass over the build output extracts everything the rest of this
	// function needs: HelmReleases below, and the inflation sources further
	// down (used to be 5 separate full passes over the same bytes).
	parsed := flux.ParseAllFromBytes(output)

	// Extract + dedup HelmReleases from the build output.
	allHRs := parsed.HelmReleases
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
			return nil, fmt.Errorf("%w: %q", errHRNotFound, name)
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
	buildRepos := parsed.HelmRepositories
	buildOCI := parsed.OCIRepositories
	buildCMs := parsed.ConfigMaps
	buildSecrets := parsed.Secrets
	rawRepos, rawOCI, rawCMs, rawSecrets := resolveHelmInflationSources(ctx, scans, clusterPath, repoRoot, quiet)

	// Merge: build-output versions are authoritative. Raw-parsed versions
	// only fill in resources NOT present in build output (by name).
	helmRepos := mergeSources(buildRepos, rawRepos, func(r flux.HelmRepository) string { return r.Metadata.Name })
	ociRepos := mergeSources(buildOCI, rawOCI, func(r flux.OCIRepository) string { return r.Metadata.Name })
	inflationCMs := mergeSources(buildCMs, rawCMs, func(c flux.ConfigMap) string { return c.Metadata.Name })
	inflationSecrets := mergeSources(buildSecrets, rawSecrets, func(s flux.Secret) string { return s.Metadata.Name })

	inflater, err := helm.NewInflater(helmCache.inflaterOptions()...)
	if err != nil {
		return nil, fmt.Errorf("initializing helm: %w", err)
	}

	return inflateAllHelmReleases(ctx, inflater, sorted, helmRepos, ociRepos, inflationCMs, inflationSecrets, inflateOptions{
		quiet:    quiet,
		strict:   strict,
		repoRoot: repoRoot,
	})
}

// errHRNotFound signals that a HelmRelease selected by name was not found.
// A sentinel so the diff comparison side can distinguish "does not exist at
// this revision" (valid added/removed diff) from build failures.
var errHRNotFound = errors.New("helmrelease not found")

// inflateOptions controls how HelmReleases are inflated.
type inflateOptions struct {
	// quiet suppresses stderr diagnostics (progress + warnings). Used by the
	// diff command's comparison side so diagnostics are not printed twice.
	quiet bool
	// strict turns skip-worthy failures (chart download/render error,
	// unresolvable chart source) into a returned error instead of a
	// warning + skip. Used by the diff command: a HelmRelease silently
	// missing from one diff side only surfaces as a false "added"/"removed"
	// diff. Deterministic skips that are documented limitations (suspend,
	// Bucket source, chartRef HelmChart) stay warnings even in strict mode —
	// they affect both diff sides equally.
	strict bool
	// repoRoot resolves GitRepository chart paths and postRenderer patches.
	repoRoot string
}

// inflateHelmReleasesShared inflates all non-suspended HelmReleases and returns
// a slice of YAML outputs. Shared by build and diff commands.
//
// In strict mode, skip-worthy failures are collected and returned as an error
// (partial output is discarded): a diff built over a state that could not be
// fully rendered would report the missing resources as added/removed.
func inflateHelmReleasesShared(ctx context.Context, inflater *helm.Inflater, helmReleases []flux.HelmRelease, helmRepos []flux.HelmRepository, ociRepos []flux.OCIRepository, configMaps []flux.ConfigMap, secrets []flux.Secret, opts inflateOptions) ([][]byte, error) {
	// stderr gates all diagnostics (progress + warnings) on !quiet. The diff
	// command runs inflation twice (current state + comparison revision);
	// without this gate the comparison side would duplicate every warning
	// already emitted by the current-state side.
	stderr := func(format string, args ...any) {
		if !opts.quiet {
			fmt.Fprintf(os.Stderr, format, args...)
		}
	}

	// fail records a skip-worthy failure. In strict mode it is collected and
	// returned as an error at the end (the loop continues so all failures are
	// reported at once); otherwise it degrades to the historical behavior:
	// warning + skip.
	var failures []string
	fail := func(hr flux.HelmRelease, reason string) {
		if opts.strict {
			failures = append(failures, fmt.Sprintf("%s/%s: %s", hr.Metadata.Namespace, hr.Metadata.Name, reason))
			return
		}
		stderr("Warning: %s for HelmRelease %s/%s — skipping\n",
			reason, hr.Metadata.Namespace, hr.Metadata.Name)
	}

	var outputs [][]byte

	// O(1) source lookups per HelmRelease instead of linear scans over the
	// repo/secret lists on every iteration. Built once per run.
	ociRepoIndex := indexByNSName(ociRepos, func(r flux.OCIRepository) flux.ObjectMeta { return r.Metadata })
	helmRepoIndex := indexByNSName(helmRepos, func(r flux.HelmRepository) flux.ObjectMeta { return r.Metadata })
	secretIndex := indexByNSName(secrets, func(s flux.Secret) flux.ObjectMeta { return s.Metadata })

	for _, hr := range helmReleases {
		if err := CheckInterrupted(ctx); err != nil {
			return nil, err
		}

		if hr.Spec.Suspend {
			stderr("Skipping suspended HelmRelease %s/%s\n", hr.Metadata.Namespace, hr.Metadata.Name)
			continue
		}

		var repoURL string
		var username string
		var password string

		// ChartRef-based HR (Flux v2 OCIRepository pattern).
		if hr.Spec.ChartRef != nil && hr.Spec.ChartRef.Kind == flux.KindOCIRepository {
			ociRef, ociVersion := resolveOCIRepoURL(hr, ociRepoIndex)
			if ociRef == "" {
				fail(hr, fmt.Sprintf("could not resolve OCIRepository source (chartRef %s/%s) — not found",
					hr.Spec.ChartRef.Namespace, hr.Spec.ChartRef.Name))
				continue
			}
			hr.Spec.Chart.Spec.Chart = ociRef
			hr.Spec.Chart.Spec.Version = ociVersion
		} else {
			if hr.Spec.Chart.Spec.Chart == "" {
				// Includes chartRef.kind=HelmChart (documented limitation) —
				// deterministic skip, warning even in strict mode.
				stderr("Warning: HelmRelease %s/%s has no chart name, skipping\n",
					hr.Metadata.Namespace, hr.Metadata.Name)
				continue
			}
			// Chart sourced from a GitRepository: the chart already lives in the
			// local checkout, so resolve it as a directory path (no network).
			// Mirrors how Kustomization.spec.path is resolved via securejoin.
			// (Bucket sources are not supported — their content lives in object
			// storage, never in the git checkout; see README limitations.)
			if sourceKind := hr.Spec.Chart.Spec.SourceRef.Kind; sourceKind == flux.KindGitRepository {
				resolved, err := securejoin.SecureJoin(opts.repoRoot, hr.Spec.Chart.Spec.Chart)
				if err != nil {
					fail(hr, fmt.Sprintf("cannot safely resolve chart path %s: %v",
						hr.Spec.Chart.Spec.Chart, err))
					continue
				}
				info, err := os.Stat(resolved)
				if err != nil {
					fail(hr, fmt.Sprintf("chart %q not found locally (%s source)",
						hr.Spec.Chart.Spec.Chart, sourceKind))
					continue
				}
				// A chart source is either a directory (Chart.yaml inside) or a
				// packaged .tgz archive. Pointing at any other kind of file is
				// almost certainly a mistake — fail early with a clear message
				// rather than letting loader.Load emit a cryptic one.
				lower := strings.ToLower(resolved)
				isArchive := strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".tar.gz")
				if !info.IsDir() && !isArchive {
					fail(hr, fmt.Sprintf("chart path %q is not a chart directory or .tgz archive (%s source)",
						hr.Spec.Chart.Spec.Chart, sourceKind))
					continue
				}
				hr.Spec.Chart.Spec.Chart = resolved
				// repoURL stays empty → InflateHelmRelease renders from the local directory.
			} else if hr.Spec.Chart.Spec.SourceRef.Kind == flux.KindBucket {
				// Bucket content lives in object storage, never in the local git
				// checkout, so it cannot be resolved offline. Emit a dedicated,
				// explicit warning (see README limitations). Deterministic skip —
				// warning even in strict mode.
				stderr("Warning: Bucket-sourced chart for HelmRelease %s/%s (chart %q) is not supported, skipping\n",
					hr.Metadata.Namespace, hr.Metadata.Name, hr.Spec.Chart.Spec.Chart)
				continue
			} else {
				repoURL, username, password = resolveHelmRepoURL(hr, helmRepoIndex, secretIndex)
				if repoURL == "" {
					fail(hr, fmt.Sprintf("could not resolve source (chart %q) — HelmRepository not found",
						hr.Spec.Chart.Spec.Chart))
					continue
				}
			}
		}

		stderr("Inflating HelmRelease %s/%s\n",
			hr.Metadata.Namespace, hr.Metadata.Name)

		output, err := inflater.InflateHelmRelease(ctx, hr, repoURL, username, password, configMaps, secrets, opts.repoRoot)
		if err != nil {
			fail(hr, fmt.Sprintf("failed to inflate: %v", err))
			continue
		}

		// Fill in metadata.namespace for resources that lack one, matching
		// `helm install --namespace <ns>` semantics (NOT
		// Kustomization.spec.targetNamespace force-override).
		//
		// Helm renders with --namespace=<ns> but does NOT inject
		// metadata.namespace into templates that don't use
		// {{ .Release.Namespace }}. Without this step, downstream
		// resource-level filters (diff hr --namespace) would drop those
		// resources ("No resources found in namespace X") even though the
		// HelmRelease was correctly inflated.
		//
		// Critically, this is **fill-missing** semantics: a resource whose
		// template hard-codes a different namespace (e.g. cert-manager
		// webhooks in kube-system, or a chart resource intentionally placed
		// in a separate namespace) is preserved as-is. Real HelmController
		// honors an explicit metadata.namespace and applies the resource
		// there — overriding it would produce a quietly-wrong diff.
		// Kustomization.spec.targetNamespace (force-override) is a different
		// mechanism for a different entity.
		//
		// We still reuse kustomize's namespace transformer (via
		// ApplyTargetNamespace) for the cluster-scoped detection — it
		// correctly skips CRDs/Namespace/ClusterRole and custom cluster-scoped
		// CRDs through kustomize's CRD registry. To combine the two, we split
		// the rendered output into "already has namespace" (preserved) and
		// "missing namespace" (run through the transformer), then merge back.
		hrNamespace := hr.Metadata.Namespace
		if hr.Spec.TargetNamespace != "" {
			hrNamespace = hr.Spec.TargetNamespace
		}
		if filled, err := applyHelmNamespace(output, hrNamespace); err == nil {
			output = filled
		} else {
			stderr("Warning: failed to fill namespace %q in HelmRelease %s/%s output: %v\n",
				hrNamespace, hr.Metadata.Namespace, hr.Metadata.Name, err)
		}

		outputs = append(outputs, output)
	}

	if len(failures) > 0 {
		if len(failures) == 1 {
			return nil, fmt.Errorf("HelmRelease %s — diff would be incomplete", failures[0])
		}
		return nil, fmt.Errorf("%d HelmReleases could not be inflated — diff would be incomplete:\n  %s",
			len(failures), strings.Join(failures, "\n  "))
	}

	return outputs, nil
}

// applyHelmNamespace fills metadata.namespace on resources that lack one,
// matching `helm install --namespace <ns>` semantics. Resources that already
// declare metadata.namespace are preserved verbatim (including those whose
// namespace differs from `namespace` — Helm honors an explicit value).
//
// To correctly skip cluster-scoped kinds (including custom cluster-scoped
// CRDs like cert-manager's), resources without metadata.namespace are passed
// through kustomize.ApplyTargetNamespace, which uses kustomize's CRD-aware
// namespace transformer. The "has namespace" group is merged back untouched.
//
// Returns input unchanged when namespace is empty or no resource needs
// filling. Errors from the kustomize transformer are propagated.
func applyHelmNamespace(data []byte, namespace string) ([]byte, error) {
	if namespace == "" {
		return data, nil
	}
	var preserve, fill []string
	for _, doc := range flux.SplitYAMLText(data) {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" {
			continue
		}
		// A doc we can't parse is preserved as-is — never silently dropped.
		var meta struct {
			Metadata struct {
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(trimmed), &meta); err != nil {
			preserve = append(preserve, doc)
			continue
		}
		if meta.Metadata.Namespace != "" {
			preserve = append(preserve, doc)
		} else {
			fill = append(fill, doc)
		}
	}
	if len(fill) == 0 {
		return data, nil
	}
	injected, err := kustomize.ApplyTargetNamespace(
		[]byte(strings.Join(fill, "\n---\n")),
		namespace,
	)
	if err != nil {
		return data, err
	}
	// Order: preserved docs first, then namespace-filled docs. Downstream
	// (buildResourceMap → diffResourceMaps) sorts by kind/namespace/name, so
	// the order here doesn't affect diff output.
	result := append(preserve, string(injected))
	return []byte(strings.Join(result, "\n---\n")), nil
}

// indexByNSName indexes resources by "namespace/name", keeping the first
// occurrence — the same winner the previous per-HelmRelease linear scans
// found. Built once per pipeline run, looked up per HelmRelease.
func indexByNSName[T any](items []T, metaOf func(T) flux.ObjectMeta) map[string]T {
	index := make(map[string]T, len(items))
	for _, item := range items {
		key := metaOf(item).Namespace + "/" + metaOf(item).Name
		if _, exists := index[key]; !exists {
			index[key] = item
		}
	}
	return index
}

// resolveOCIRepoURL finds the OCIRepository reference for a HelmRelease's chartRef
// in the pre-built namespace/name index. Returns (chartRef, version) where
// chartRef is the full OCI reference (URL, optionally with @digest appended),
// and version is semver/tag.
func resolveOCIRepoURL(hr flux.HelmRelease, ociRepos map[string]flux.OCIRepository) (string, string) {
	if hr.Spec.ChartRef == nil {
		return "", ""
	}
	repoNS := hr.Spec.ChartRef.Namespace
	if repoNS == "" {
		repoNS = hr.Metadata.Namespace
	}
	repo, ok := ociRepos[repoNS+"/"+hr.Spec.ChartRef.Name]
	if !ok {
		return "", ""
	}
	url := repo.Spec.URL
	ref := repo.Spec.Ref

	if ref.HasDigest() {
		return url + "@" + ref.Digest, ""
	}

	return url, ref.ResolveVersion()
}

// resolveHelmRepoURL finds the HelmRepository URL for a HelmRelease's chart in
// the pre-built namespace/name indexes.
func resolveHelmRepoURL(hr flux.HelmRelease, helmRepos map[string]flux.HelmRepository, secrets map[string]flux.Secret) (string, string, string) {
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

// parseWithRootFallback parses one resource type needed for HelmRelease
// inflation from clusterPath, falling back to repoRoot when the cluster path
// yields none (sources may live outside it, e.g. a shared sources/ or
// flux-system/ directory). The parse closure receives the snapshot-cached
// parser for the root being walked, so each tree is parsed at most once per
// command regardless of how many resource types are resolved.
func parseWithRootFallback[T any](scans *scanCache, clusterPath, repoRoot, label string, parse func(*flux.Parser) ([]T, error), stderr func(format string, args ...any)) []T {
	items, err := parse(scans.parserFor(clusterPath))
	if err != nil {
		stderr("Warning: could not parse %s: %v\n", label, err)
		items = nil
	}
	if len(items) == 0 && repoRoot != "" && repoRoot != clusterPath {
		if rootItems, rErr := parse(scans.parserFor(repoRoot)); rErr != nil {
			stderr("Warning: could not parse %s from %s: %v\n", label, repoRoot, rErr)
		} else {
			items = rootItems
		}
	}
	return items
}

// resolveHelmInflationSources parses the source resources needed for HelmRelease
// inflation (HelmRepository, OCIRepository, ConfigMap, Secret). Each resource
// type is first parsed from clusterPath; if none are found there, the search
// falls back to repoRoot so that sources defined outside the cluster path
// (e.g. a shared sources/ or flux-system/ directory) are still resolved.
func resolveHelmInflationSources(ctx context.Context, scans *scanCache, clusterPath, repoRoot string, quiet bool) (helmRepos []flux.HelmRepository, ociRepos []flux.OCIRepository, configMaps []flux.ConfigMap, secrets []flux.Secret) {
	stderr := func(format string, args ...any) {
		if !quiet {
			fmt.Fprintf(os.Stderr, format, args...)
		}
	}

	// All four parse closures share one cached snapshot per root: the
	// clusterPath walk happens once (already done for ParseKustomizations by
	// the caller), and the repoRoot fallback walk at most once.
	helmRepos = parseWithRootFallback(scans, clusterPath, repoRoot, "HelmRepositories",
		func(p *flux.Parser) ([]flux.HelmRepository, error) { return p.ParseHelmRepositories(ctx) }, stderr)
	ociRepos = parseWithRootFallback(scans, clusterPath, repoRoot, "OCIRepositories",
		func(p *flux.Parser) ([]flux.OCIRepository, error) { return p.ParseOCIRepositories(ctx) }, stderr)
	configMaps = parseWithRootFallback(scans, clusterPath, repoRoot, "ConfigMaps",
		func(p *flux.Parser) ([]flux.ConfigMap, error) { return p.ParseConfigMaps(ctx) }, stderr)
	secrets = parseWithRootFallback(scans, clusterPath, repoRoot, "Secrets",
		func(p *flux.Parser) ([]flux.Secret, error) { return p.ParseSecrets(ctx) }, stderr)

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
