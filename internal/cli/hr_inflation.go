package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"gopkg.in/yaml.v3"

	"github.com/banschikovde/fluxview/internal/cachedir"
	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/gitsource"
	"github.com/banschikovde/fluxview/internal/helm"
	"github.com/banschikovde/fluxview/internal/kustomize"
	"github.com/cyphar/filepath-securejoin"
)

// helmCacheOptions carries Helm cache settings from CLI flags/env into the
// Inflater: where the on-disk Helm cache lives, how long repository
// indexes stay fresh, and how long one HTTP download may take (0 = no
// limit). The dir value "disabled" disables reuse (per-process
// temp dir, removed by Inflater.Close) — the same spell as every other
// fluxview cache. Independent of the kustomize cache.
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
// conventions: dir "disabled" disables the cache, ttl 0
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
	*cacheDir = helm.DefaultCacheDir() // pre-seed: pflag.Var does not set defaults
	cmd.Flags().Var(cachedir.NewFlag(cacheDir), "helm-cache-dir",
		"Helm cache directory for repo indexes and downloaded charts; \"disabled\" disables reuse (per-process temp dir) (env: FLUXVIEW_HELM_CACHE_DIR)")
	cmd.Flags().DurationVar(indexTTL, "helm-index-ttl", helm.DefaultIndexTTL(),
		"How long cached Helm repo indexes and OCI tag resolutions stay fresh; 0 always refreshes (env: FLUXVIEW_HELM_INDEX_TTL)")
	cmd.Flags().DurationVar(downloadTimeout, "helm-download-timeout", helm.DefaultDownloadTimeout(),
		"Per-request timeout for downloading Helm repo indexes and chart tarballs; 0 = no limit, for slow networks (env: FLUXVIEW_HELM_DOWNLOAD_TIMEOUT)")
}

// registerKustomizeCacheFlags registers the kustomize cache flags shared by
// the commands, following one convention for every fluxview cache:
//
//	--<name>-cache-dir  cache directory; "disabled" disables
//	--<name>-cache-ttl  freshness window; 0 always bypasses (still writes)
//
// Here <name> is "remote" (resources fetched by URL, referenced by
// kustomizations), "build" (kustomize build outputs) and "git-source"
// (clones of external GitRepository sources). Defaults come from
// kustomize.DefaultRemoteCacheDir()/DefaultRemoteCacheTTL()/
// DefaultRemoteCacheTimeout()/DefaultBuildCacheDir()/DefaultBuildCacheTTL()/
// gitsource.DefaultCacheDir()/DefaultTTL().
func registerKustomizeCacheFlags(cmd *cobra.Command, opts *kustomizeCacheOptions) {
	*opts = kustomizeCacheOptions{
		remoteDir:     kustomize.DefaultRemoteCacheDir(), // pre-seed: pflag.Var does not set defaults
		remoteTtl:     kustomize.DefaultRemoteCacheTTL(),
		remoteTimeout: kustomize.DefaultRemoteCacheTimeout(),
		buildCacheDir: kustomize.DefaultBuildCacheDir(),
		buildCacheTTL: kustomize.DefaultBuildCacheTTL(),
		gitSourceDir:  gitsource.DefaultCacheDir(),
		gitSourceTtl:  gitsource.DefaultTTL(),
	}
	cmd.Flags().Var(cachedir.NewFlag(&opts.remoteDir), "remote-cache-dir",
		"Cache directory for remote resources referenced by kustomizations; \"disabled\" disables (env: FLUXVIEW_REMOTE_CACHE_DIR)")
	cmd.Flags().DurationVar(&opts.remoteTtl, "remote-cache-ttl", kustomize.DefaultRemoteCacheTTL(),
		"How long cached remote resources with floating refs (branch/HEAD URLs) stay fresh; pinned version URLs never expire; 0 always re-fetches (env: FLUXVIEW_REMOTE_CACHE_TTL)")
	cmd.Flags().DurationVar(&opts.remoteTimeout, "remote-cache-timeout", kustomize.DefaultRemoteCacheTimeout(),
		"Per-request timeout for downloading remote resources referenced by kustomizations; 0 = no limit, for slow networks (env: FLUXVIEW_REMOTE_CACHE_TIMEOUT)")
	cmd.Flags().Var(cachedir.NewFlag(&opts.buildCacheDir), "kustomize-build-cache-dir",
		"Cache directory for kustomize build outputs, reused while input files are unchanged; \"disabled\" disables (env: FLUXVIEW_KUSTOMIZE_BUILD_CACHE_DIR)")
	cmd.Flags().DurationVar(&opts.buildCacheTTL, "kustomize-build-cache-ttl", kustomize.DefaultBuildCacheTTL(),
		"How long cached kustomize build outputs stay usable; 0 always rebuilds (entries are still refreshed) (env: FLUXVIEW_KUSTOMIZE_BUILD_CACHE_TTL)")
	cmd.Flags().Var(cachedir.NewFlag(&opts.gitSourceDir), "git-source-cache-dir",
		"Cache directory for clones of external GitRepository sources; \"disabled\" disables reuse (env: FLUXVIEW_GIT_SOURCE_CACHE_DIR)")
	cmd.Flags().DurationVar(&opts.gitSourceTtl, "git-source-cache-ttl", gitsource.DefaultTTL(),
		"How long floating external source resolutions (branch/semver/HEAD) stay fresh; pinned commit/tag clones never expire; 0 always re-resolves (env: FLUXVIEW_GIT_SOURCE_CACHE_TTL)")
}

// registerGitSourceAuthFlags registers the external git source auth flags
// shared by the commands — policy knobs only. The credential variables
// (FLUXVIEW_GIT_SSH_KEY, FLUXVIEW_GIT_SSH_PASSPHRASE,
// FLUXVIEW_GIT_USERNAME/PASSWORD, FLUXVIEW_GIT_TOKEN) and their host
// allowlist (FLUXVIEW_GIT_CREDENTIAL_HOSTS) deliberately have no flags:
// secrets must not appear on command lines. Defaults come from the
// environment, so an explicit flag overrides the env var per run.
func registerGitSourceAuthFlags(cmd *cobra.Command, knownHosts *string, acceptNew *bool) {
	cmd.Flags().StringVar(knownHosts, "git-source-ssh-known-hosts", gitsource.DefaultSSHKnownHostsFile(),
		"known_hosts file for SSH host key verification of external git sources; empty = ~/.ssh/known_hosts plus /etc/ssh/ssh_known_hosts (env: FLUXVIEW_GIT_SSH_KNOWN_HOSTS)")
	cmd.Flags().BoolVar(acceptNew, "git-source-ssh-accept-new", gitsource.DefaultSSHAcceptNew(),
		"Accept unknown SSH host keys of external git sources on first use (TOFU; remembered in memory for the run, known_hosts files are never written) (env: FLUXVIEW_GIT_SSH_ACCEPT_NEW)")
}

// applyGitSourceAuthFlags pushes explicitly passed git-source auth flags
// into the environment the auth resolver reads. Defaults already come
// from the env, so only flags the user actually set (Changed) are
// exported; direct run* callers keep full control of the environment.
// Keeps NewFetcher's signature and the env-only credential channel
// intact.
func applyGitSourceAuthFlags(flags *pflag.FlagSet, knownHosts string, acceptNew bool) {
	if flags.Changed("git-source-ssh-known-hosts") {
		os.Setenv(gitsource.EnvSSHKnownHosts, knownHosts)
	}
	if flags.Changed("git-source-ssh-accept-new") {
		os.Setenv(gitsource.EnvSSHAcceptNew, strconv.FormatBool(acceptNew))
	}
}

// helmFleet is the discovery stage output shared by build hr / diff (chart
// inflation) and inventory: the combined Kustomization build output plus
// everything chart resolution needs, parsed from it and the raw tree. The
// output also feeds the inventory collectors for manifests, CRs and CRDs.
type helmFleet struct {
	output []byte
	// kustomizations are the Flux Kustomizations the output was built from
	// (inventory attribution).
	kustomizations []flux.Kustomization
	// helmReleases are deduped and dependency-sorted.
	helmReleases []flux.HelmRelease
	helmRepos    []flux.HelmRepository
	ociRepos     []flux.OCIRepository
	gitRepos     []flux.GitRepository
	configMaps   []flux.ConfigMap
	secrets      []flux.Secret
}

// discoverHelmFleet runs the Kustomization pipeline over clusterPath once:
// builds all Flux Kustomizations, parses the combined output and resolves
// the chart sources (build output authoritative, raw-parsed fallback — see
// mergeSources). Returns a nil fleet when the path holds no Flux
// Kustomizations — valid for the diff comparison state, an error for
// commands that require them.
func discoverHelmFleet(ctx context.Context, scans *scanCache, clusterPath, repoRoot string, quiet bool, ksCache kustomizeCacheOptions, gitSources *gitSourceEnv) (*helmFleet, error) {
	kustomizations, err := scans.parserFor(clusterPath).ParseKustomizations(ctx)
	if err != nil {
		return nil, nil // no Flux KS — valid for diff
	}

	builder := kustomize.NewBuilder(repoRoot, ksCache.builderOptions()...)
	buildCache := make(buildCache)
	configMaps := resolveConfigMaps(ctx, scans, clusterPath, builder, buildCache)
	secrets := resolveSecrets(ctx, scans, clusterPath, builder, buildCache)

	output, err := buildKSContent(ctx, &ksBuildEnv{
		scans:       scans,
		builder:     builder,
		repoRoot:    repoRoot,
		clusterPath: clusterPath,
		quiet:       true,
		cache:       buildCache,
		gitSources:  gitSources,
	}, kustomizations, substitutionSources{configMaps: configMaps, secrets: secrets})
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
	rawRepos, rawOCI, rawCMs, rawSecrets := resolveHelmInflationSources(ctx, scans, clusterPath, repoRoot, quiet)
	rawGit := parseWithRootFallback(scans, clusterPath, repoRoot, "GitRepositories",
		func(p *flux.Parser) ([]flux.GitRepository, error) { return p.ParseGitRepositories(ctx) },
		func(format string, args ...any) {
			if !quiet {
				fmt.Fprintf(os.Stderr, format, args...)
			}
		})

	// Merge: build-output versions are authoritative. Raw-parsed versions
	// only fill in resources NOT present in build output (by name).
	return &helmFleet{
		output:         output,
		kustomizations: kustomizations,
		helmReleases:   sorted,
		helmRepos:      mergeSources(parsed.HelmRepositories, rawRepos, func(r flux.HelmRepository) string { return r.Metadata.Name }),
		ociRepos:       mergeSources(parsed.OCIRepositories, rawOCI, func(r flux.OCIRepository) string { return r.Metadata.Name }),
		gitRepos:       mergeSources(parsed.GitRepositories, rawGit, func(r flux.GitRepository) string { return r.Metadata.Name }),
		configMaps:     mergeSources(parsed.ConfigMaps, rawCMs, func(c flux.ConfigMap) string { return c.Metadata.Name }),
		secrets:        mergeSources(parsed.Secrets, rawSecrets, func(s flux.Secret) string { return s.Metadata.Name }),
	}, nil
}

// hrInflationEnv is the shared state of one command's HelmRelease inflation
// runs: memoized scans, cache settings and the external git source env. The
// diff command builds one env and runs both sides (current state + comparison
// revision) against it — only the paths and the quiet knob differ per side.
type hrInflationEnv struct {
	scans      *scanCache
	strict     bool
	helmCache  helmCacheOptions
	ksCache    kustomizeCacheOptions
	gitSources *gitSourceEnv
}

// buildHRInflation discovers HelmReleases through the Flux Kustomization pipeline
// (same discovery logic as runBuildHR), resolves sources, inflates the charts,
// and returns combined YAML. Returns nil if no Flux Kustomizations or no
// HelmReleases are found (valid for diff comparison state).
//
// namespace filters the HelmRelease list BEFORE inflation — when set, only
// matching HRs are inflated, avoiding unnecessary chart downloads.
//
// env.strict (diff mode) turns skip-worthy inflation failures into a returned
// error instead of a warning + skip, so an unbuildable state never produces a
// misleading partial diff; quiet suppresses this run's diagnostics.
func buildHRInflation(ctx context.Context, env *hrInflationEnv, clusterPath, repoRoot, name, namespace string, quiet bool) ([]byte, error) {
	scans, gitSources := env.scans, env.gitSources
	fleet, err := discoverHelmFleet(ctx, scans, clusterPath, repoRoot, quiet, env.ksCache, gitSources)
	if err != nil {
		return nil, err
	}
	if fleet == nil {
		return nil, nil
	}

	helmReleases := fleet.helmReleases

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

	inflater, err := helm.NewInflater(env.helmCache.inflaterOptions()...)
	if err != nil {
		return nil, fmt.Errorf("initializing helm: %w", err)
	}
	// A cache-disabled run (--helm-cache-dir=off) inflated from a temp
	// directory; drop it. A persisted cache is a no-op close.
	defer inflater.Close()

	return inflateAllHelmReleases(ctx, inflater, helmInflationInput{
		helmReleases: helmReleases,
		helmRepos:    fleet.helmRepos,
		ociRepos:     fleet.ociRepos,
		configMaps:   fleet.configMaps,
		secrets:      fleet.secrets,
	}, inflateOptions{
		quiet:    quiet,
		strict:   env.strict,
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

// helmInflationInput is the Flux resource set one Helm inflation run renders:
// the HelmReleases plus the repositories they resolve charts against and the
// ConfigMaps/Secrets their values reference.
type helmInflationInput struct {
	helmReleases []flux.HelmRelease
	helmRepos    []flux.HelmRepository
	ociRepos     []flux.OCIRepository
	configMaps   []flux.ConfigMap
	secrets      []flux.Secret
}

// inflateHelmReleasesShared inflates all non-suspended HelmReleases and returns
// a slice of YAML outputs. Shared by build and diff commands.
//
// In strict mode, skip-worthy failures are collected and returned as an error
// (partial output is discarded): a diff built over a state that could not be
// fully rendered would report the missing resources as added/removed.
func inflateHelmReleasesShared(ctx context.Context, inflater *helm.Inflater, input helmInflationInput, opts inflateOptions) ([][]byte, error) {
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
	ociRepoIndex := indexByNSName(input.ociRepos, func(r flux.OCIRepository) flux.ObjectMeta { return r.Metadata })
	helmRepoIndex := indexByNSName(input.helmRepos, func(r flux.HelmRepository) flux.ObjectMeta { return r.Metadata })
	secretIndex := indexByNSName(input.secrets, func(s flux.Secret) flux.ObjectMeta { return s.Metadata })

	for _, hr := range input.helmReleases {
		if err := CheckInterrupted(ctx); err != nil {
			return nil, err
		}

		if hr.Spec.Suspend {
			stderr("Skipping suspended HelmRelease %s/%s\n", hr.Metadata.Namespace, hr.Metadata.Name)
			continue
		}

		target, ok := resolveHRChartTarget(&hr, input, sourceIndexes{oci: ociRepoIndex, helm: helmRepoIndex, secret: secretIndex}, opts.repoRoot, fail, stderr)
		if !ok {
			continue
		}

		stderr("Inflating HelmRelease %s/%s\n",
			hr.Metadata.Namespace, hr.Metadata.Name)

		output, err := inflater.InflateHelmRelease(ctx, hr, target.repoURL,
			helm.ChartCredentials{Username: target.username, Password: target.password},
			input.configMaps, input.secrets, opts.repoRoot)
		if err != nil {
			fail(hr, fmt.Sprintf("failed to inflate: %v", err))
			continue
		}

		outputs = append(outputs, fillHelmNamespace(output, hr, stderr))
	}

	if err := strictFailuresError(failures); err != nil {
		return nil, err
	}

	return outputs, nil
}

// hrChartTarget is where one HelmRelease's chart comes from after source
// resolution: the classic repo URL with its credentials. A zero value
// means the chart reference is self-contained (local path or OCI ref
// inside the mutated chart spec — InflateHelmRelease resolves it).
type hrChartTarget struct {
	repoURL  string
	username string
	password string
}

// hrFailFunc records one skip-worthy HelmRelease failure (strict mode
// collects it, lenient mode warns); hrStderrFunc prints a diagnostic
// gated on !quiet. Shared by the chart-source resolver so its branches
// report exactly like the surrounding loop.
type (
	hrFailFunc   func(hr flux.HelmRelease, reason string)
	hrStderrFunc func(format string, args ...any)
)

// resolveHRChartTarget resolves one HelmRelease's chart source into an
// inflation target, mutating hr.Spec.Chart.Spec in place for the
// self-contained shapes: the OCIRepository chartRef pattern writes the
// resolved OCI ref, a GitRepository chart source writes the resolved
// local directory/archive path (the chart already lives in the checkout —
// no network; mirrors how Kustomization.spec.path is resolved via
// securejoin). ok=false means skip — the reason was already reported.
func resolveHRChartTarget(
	hr *flux.HelmRelease,
	input helmInflationInput,
	idx sourceIndexes,
	repoRoot string,
	fail hrFailFunc,
	stderr hrStderrFunc,
) (hrChartTarget, bool) {
	ociRepoIndex, helmRepoIndex, secretIndex := idx.oci, idx.helm, idx.secret
	// ChartRef-based HR (Flux v2 OCIRepository pattern).
	if hr.Spec.ChartRef != nil && hr.Spec.ChartRef.Kind == flux.KindOCIRepository {
		ociRef, ociVersion := resolveOCIRepoURL(*hr, ociRepoIndex)
		if ociRef == "" {
			fail(*hr, fmt.Sprintf("could not resolve OCIRepository source (chartRef %s/%s) — not found",
				hr.Spec.ChartRef.Namespace, hr.Spec.ChartRef.Name))
			return hrChartTarget{}, false
		}
		hr.Spec.Chart.Spec.Chart = ociRef
		hr.Spec.Chart.Spec.Version = ociVersion
		return hrChartTarget{}, true
	}

	if hr.Spec.Chart.Spec.Chart == "" {
		// Includes chartRef.kind=HelmChart (documented limitation) —
		// deterministic skip, warning even in strict mode.
		stderr("Warning: HelmRelease %s/%s has no chart name, skipping\n",
			hr.Metadata.Namespace, hr.Metadata.Name)
		return hrChartTarget{}, false
	}

	switch hr.Spec.Chart.Spec.SourceRef.Kind {
	case flux.KindGitRepository:
		return resolveLocalChartTarget(hr, repoRoot, fail)
	case flux.KindBucket:
		// Bucket content lives in object storage, never in the local git
		// checkout, so it cannot be resolved offline. Emit a dedicated,
		// explicit warning (see README limitations). Deterministic skip —
		// warning even in strict mode.
		stderr("Warning: Bucket-sourced chart for HelmRelease %s/%s (chart %q) is not supported, skipping\n",
			hr.Metadata.Namespace, hr.Metadata.Name, hr.Spec.Chart.Spec.Chart)
		return hrChartTarget{}, false
	}

	repoURL, username, password := resolveHelmRepoURL(*hr, helmRepoIndex, secretIndex)
	if repoURL == "" {
		fail(*hr, fmt.Sprintf("could not resolve source (chart %q) — HelmRepository not found",
			hr.Spec.Chart.Spec.Chart))
		return hrChartTarget{}, false
	}
	return hrChartTarget{repoURL: repoURL, username: username, password: password}, true
}

// resolveLocalChartTarget resolves a GitRepository chart source to the
// chart's local path (directory with Chart.yaml or a packaged .tgz
// archive) inside the checkout, writing it into hr.Spec.Chart.Spec.Chart
// — repoURL stays empty, InflateHelmRelease renders from the local path.
func resolveLocalChartTarget(hr *flux.HelmRelease, repoRoot string, fail hrFailFunc) (hrChartTarget, bool) {
	resolved, err := securejoin.SecureJoin(repoRoot, hr.Spec.Chart.Spec.Chart)
	if err != nil {
		fail(*hr, fmt.Sprintf("cannot safely resolve chart path %s: %v",
			hr.Spec.Chart.Spec.Chart, err))
		return hrChartTarget{}, false
	}
	info, err := os.Stat(resolved)
	if err != nil {
		fail(*hr, fmt.Sprintf("chart %q not found locally (%s source)",
			hr.Spec.Chart.Spec.Chart, flux.KindGitRepository))
		return hrChartTarget{}, false
	}
	// A chart source is either a directory (Chart.yaml inside) or a
	// packaged .tgz archive. Pointing at any other kind of file is almost
	// certainly a mistake — fail early with a clear message rather than
	// letting loader.Load emit a cryptic one.
	lower := strings.ToLower(resolved)
	isArchive := strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".tar.gz")
	if !info.IsDir() && !isArchive {
		fail(*hr, fmt.Sprintf("chart path %q is not a chart directory or .tgz archive (%s source)",
			hr.Spec.Chart.Spec.Chart, flux.KindGitRepository))
		return hrChartTarget{}, false
	}
	hr.Spec.Chart.Spec.Chart = resolved
	return hrChartTarget{}, true
}

// fillHelmNamespace fills metadata.namespace on resources that lack one
// (see applyHelmNamespace for the fill-missing semantics and why real
// HelmController behavior must be preserved); a transformer failure warns
// and keeps the unfilled output.
func fillHelmNamespace(output []byte, hr flux.HelmRelease, stderr hrStderrFunc) []byte {
	hrNamespace := hr.Metadata.Namespace
	if hr.Spec.TargetNamespace != "" {
		hrNamespace = hr.Spec.TargetNamespace
	}
	filled, err := applyHelmNamespace(output, hrNamespace)
	if err != nil {
		stderr("Warning: failed to fill namespace %q in HelmRelease %s/%s output: %v\n",
			hrNamespace, hr.Metadata.Namespace, hr.Metadata.Name, err)
		return output
	}
	return filled
}

// strictFailuresError turns collected strict-mode failures into the loop's
// error: partial output would make a diff report the missing resources as
// added/removed, so everything is reported at once and the run fails.
func strictFailuresError(failures []string) error {
	if len(failures) == 0 {
		return nil
	}
	if len(failures) == 1 {
		return fmt.Errorf("HelmRelease %s — diff would be incomplete", failures[0])
	}
	return fmt.Errorf("%d HelmReleases could not be inflated — diff would be incomplete:\n  %s",
		len(failures), strings.Join(failures, "\n  "))
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
		[]byte(strings.Join(fill, sectionSeparator)),
		namespace,
	)
	if err != nil {
		return data, err
	}
	// Order: preserved docs first, then namespace-filled docs. Downstream
	// (buildResourceMap → diffResourceMaps) sorts by kind/namespace/name, so
	// the order here doesn't affect diff output.
	result := append(preserve, string(injected))
	return []byte(strings.Join(result, sectionSeparator)), nil
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
