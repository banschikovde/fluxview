package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/banschikovde/fluxview/internal/cachedir"
	"github.com/banschikovde/fluxview/internal/git"
	"github.com/banschikovde/fluxview/internal/kustomize"
	"github.com/banschikovde/fluxview/internal/validate"
)

// kubeconformLocalTemplate is the filename template of kubeconform local
// schema directories: <kind>-<group>-<version>.json at the directory root
// (the layout of Flux crd-schemas.tar.gz and our converted CRD schemas).
const kubeconformLocalTemplate = "{{ .ResourceKind }}{{ .KindSuffix }}.json"

// kubeconformVersionedTemplate is the directory template of a
// kubernetes-json-schema checkout: v<version>-standalone[-strict]/ subdirs
// with <kind>-<group>-<version>.json inside — the same layout the default
// HTTP registry serves (NormalizedKubernetesVersion carries the "v"
// prefix), addressed locally for fully offline validation.
const kubeconformVersionedTemplate = "{{ .NormalizedKubernetesVersion }}-standalone{{ .StrictSuffix }}/{{ .ResourceKind }}{{ .KindSuffix }}.json"

// ValidateFlags holds flags for the validate command.
type ValidateFlags struct {
	Path              string
	Namespace         string
	SchemaDir         string
	KubernetesVersion string
	Strict            bool
	SkipKinds         []string
	Output            string
	// SchemaDownloadTimeout bounds one schema download attempt; 0 means
	// no per-request limit (the run stays interruptible via Ctrl-C).
	SchemaDownloadTimeout time.Duration
	// KsCache groups the kustomize cache flags (remote resources, build
	// outputs, external git source clones) — populated by
	// registerKustomizeCacheFlags.
	KsCache kustomizeCacheOptions
	// SchemaCacheDir and CRDSchemaCacheDir override the validate-side
	// schema caches (downloaded schemas, converted CRDs); the disable spell
	// turns each into a per-run temp cache (see runValidate).
	SchemaCacheDir    string
	CRDSchemaCacheDir string
	// GitSourceSSHKnownHosts and GitSourceSSHAcceptNew are the external
	// git source auth policy knobs (credentials stay env-only).
	GitSourceSSHKnownHosts string
	GitSourceSSHAcceptNew  bool
	// NoGitSourceFetch disables cloning external GitRepository sources
	// (--no-git-source-fetch): such Kustomizations are named in a warning
	// and left unchecked by the gate.
	NoGitSourceFetch bool

	// disableDefaultSchemas drops the default HTTP schema registry. It has
	// no flag — only tests set it, to keep them offline.
	disableDefaultSchemas bool
	// testRegistryURL points the schema prefetch at a test HTTP server;
	// testCacheBase overrides the fluxview cache base (schemas, CRD
	// conversion). No flags — tests only.
	testRegistryURL string
	testCacheBase   string
}

// schemaCacheDir resolves the downloaded-schema cache directory: an explicit
// --schema-cache-dir (or FLUXVIEW_SCHEMA_CACHE_DIR), else the test override,
// else the default. The value may still be the disable spell — runValidate
// resolves it via validate.SchemaCacheDirOrTemp.
func (f *ValidateFlags) schemaCacheDir() string {
	if f.testCacheBase != "" {
		return filepath.Join(f.testCacheBase, "schemas")
	}
	if f.SchemaCacheDir != "" {
		return f.SchemaCacheDir
	}
	return validate.DefaultSchemaCacheDir()
}

// crdSchemaCacheDir resolves the CRD conversion cache directory the same way.
// The disable spell maps to an empty cacheRoot (a per-run temp directory
// inside validate.CRDYAMLToSchemaDir, removed by runValidate after
// validation).
func (f *ValidateFlags) crdSchemaCacheDir() string {
	if f.testCacheBase != "" {
		return filepath.Join(f.testCacheBase, "crd-schemas")
	}
	if f.CRDSchemaCacheDir != "" {
		return f.CRDSchemaCacheDir
	}
	return validate.DefaultCRDSchemaCacheDir()
}

// schemaDownloadTimeout translates the flag value for the library: the
// flag's 0 means "no per-request limit", which the library spells as a
// negative timeout.
func schemaDownloadTimeout(flagValue time.Duration) time.Duration {
	if flagValue == 0 {
		return -1
	}
	return flagValue
}

func newValidateCmd() *cobra.Command {
	flags := &ValidateFlags{}

	cmd := &cobra.Command{
		Use:   "validate [flags]",
		Short: "Validate Flux resources against schemas",
		Long: `Validate built Flux resources against schemas (kubeconform engine).

Kubernetes resources are validated against schemas for --kubernetes-version,
downloaded on first use with bounded timeouts and cached locally. Flux CRDs
and other custom resources need schemas from --schema-dir (default: /schemas/
or ./schemas/): kubeconform JSON files (e.g. Flux crd-schemas.tar.gz
contents) and/or CRD YAML manifests. A kubernetes-json-schema checkout (dirs
named v<version>-standalone[-strict]) in or one level under --schema-dir
validates native kinds fully offline. Resources without a matching schema
are silently skipped; --skip-kind excludes kinds explicitly. --output json
or junit emits a machine-readable report (with per-resource statuses and a
summary) to stdout.

Examples:
  fluxview validate --path clusters/prod/
  fluxview validate --path clusters/prod/ --schema-dir ./schemas
  fluxview validate --path clusters/prod/ --kubernetes-version 1.34.0
  fluxview validate --path clusters/prod/ --strict
  fluxview validate --path clusters/prod/ --output json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			applyGitSourceAuthFlags(cmd.Flags(), flags.GitSourceSSHKnownHosts, flags.GitSourceSSHAcceptNew)
			return runValidate(cmd.Context(), flags)
		},
	}

	cmd.Flags().StringVarP(&flags.Path, "path", "p", "", "Path to the cluster directory in the repository")
	cmd.Flags().StringVarP(&flags.Namespace, "namespace", "n", "", "Filter output resources by namespace")
	cmd.Flags().StringVar(&flags.SchemaDir, "schema-dir", "", "Directory with schemas: kubeconform JSON files, CRD YAML manifests and/or a kubernetes-json-schema checkout (default: /schemas/ or ./schemas/)")
	cmd.Flags().StringVar(&flags.KubernetesVersion, "kubernetes-version", validate.DefaultKubernetesVersion, "Kubernetes version (full X.Y.Z like 1.36.1, or master) for the default schema location")
	cmd.Flags().DurationVar(&flags.SchemaDownloadTimeout, "schema-download-timeout", validate.DefaultSchemaDownloadTimeout, "Per-request timeout for downloading Kubernetes schemas (e.g. 30s, 1m; 0 = no limit)")
	cmd.Flags().BoolVar(&flags.Strict, "strict", false, "Reject duplicated YAML keys; strict schemas for the default registry also reject unknown fields")
	cmd.Flags().StringSliceVar(&flags.SkipKinds, "skip-kind", nil, "Kinds to skip (repeatable or comma-separated): Kind (e.g. Deployment, any apiVersion) or apiVersion/Kind (e.g. apps/v1/Deployment)")
	cmd.Flags().StringVar(&flags.Output, "output", "text", "Output format: text, json or junit (machine formats go to stdout)")
	registerKustomizeCacheFlags(cmd, &flags.KsCache)
	registerNoGitSourceFetchFlag(cmd, &flags.NoGitSourceFetch)
	flags.SchemaCacheDir = validate.DefaultSchemaCacheDir() // pre-seed: pflag.Var does not set defaults
	cmd.Flags().Var(cachedir.NewFlag(&flags.SchemaCacheDir), "schema-cache-dir",
		"Cache directory for schemas downloaded from the default registry, version-pinned; \"disabled\" disables reuse (per-run temp dir) (env: FLUXVIEW_SCHEMA_CACHE_DIR)")
	flags.CRDSchemaCacheDir = validate.DefaultCRDSchemaCacheDir() // pre-seed: pflag.Var does not set defaults
	cmd.Flags().Var(cachedir.NewFlag(&flags.CRDSchemaCacheDir), "crd-schema-cache-dir",
		"Cache directory for schemas converted from CRD YAML manifests; \"disabled\" disables reuse (reconverted per run) (env: FLUXVIEW_CRD_SCHEMA_CACHE_DIR)")
	registerGitSourceAuthFlags(cmd, &flags.GitSourceSSHKnownHosts, &flags.GitSourceSSHAcceptNew)

	return cmd
}

func runValidate(ctx context.Context, flags *ValidateFlags) error {
	if err := checkValidateFlags(flags); err != nil {
		return err
	}

	absClusterPath, repoRoot, err := resolveValidatePaths(flags)
	if err != nil {
		return err
	}

	// Resolve the schema directory.
	schemaDir := flags.SchemaDir
	if schemaDir == "" {
		schemaDir = defaultSchemaDir()
	}

	// Schema caches: the disable word turns each into a per-run temp cache.
	// Both cleanups must run after validation (kubeconform reads schemas
	// lazily) — plain defers at this function level are exactly that.
	schemaCacheDir, crdCacheRoot, cleanupCaches, err := resolveValidateCaches(flags)
	if err != nil {
		return err
	}
	defer cleanupCaches()

	// Announce the validation context before the (possibly slow) build, so
	// it is clear what resources will be validated against while it runs.
	announceValidationContext(flags, schemaDir)

	output, buildState, err := buildValidationOutput(ctx, flags, absClusterPath, repoRoot)
	if err != nil {
		return err
	}
	defer buildState.close()

	// A validation gate must not pass on a partial build: kustomize build
	// failures, KS paths missing from the repository, or unfetchable
	// external sources all leave resources silently absent from the
	// checked set. The first two fail the run; unfetchable external
	// content is best-effort (warn) — failing would abort every
	// --no-git-source-fetch run outright.
	if err := gatePartialBuild(buildState); err != nil {
		return err
	}
	warnUncheckedKustomizations(buildState.report)

	if output == nil {
		fmt.Fprintln(os.Stderr, "No resources to validate.")
		return nil
	}

	// Check for interruption before validation
	if err := CheckInterrupted(ctx); err != nil {
		return err
	}

	// CRDs are schema definitions, not resources to validate — drop them,
	// then apply the namespace filter (an empty result is a normal exit).
	output = filterCRDDocs(output)
	output, done := filterValidationOutput(flags, output)
	if done {
		return nil
	}

	locations, coverage, err := composeSchemaLocations(flags, schemaDir, schemaCacheDir, crdCacheRoot)
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}

	// kubeconform's HTTP loader has no timeout and takes no context, so a
	// hung registry would hang validation forever. Instead, prefetch the
	// schemas it would request under a cancellable client with per-request
	// timeouts; validation then reads them from the local cache.
	if err := prefetchSchemas(ctx, flags, output, coverage, schemaCacheDir); err != nil {
		return err
	}

	validator, err := validate.New(validate.Options{
		SchemaLocations:   locations,
		KubernetesVersion: flags.KubernetesVersion,
		// Downloaded schemas are version-pinned and immutable — cache them
		// unconditionally; the cache only saves network round-trips.
		CacheDir:  schemaCacheDir,
		Strict:    flags.Strict,
		SkipKinds: flags.SkipKinds,
	})
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}

	results := validator.ValidateAll(output)
	return emitValidationReport(flags, results, validate.Failures(results))
}

// announceValidationContext prints what validation will run against,
// before the (possibly slow) build — while it runs it stays clear which
// schemas the resources are checked with.
func announceValidationContext(flags *ValidateFlags, schemaDir string) {
	planned := plannedSchemaDisplay(flags, schemaDir)
	if planned == "" {
		return
	}
	if flags.disableDefaultSchemas {
		fmt.Fprintf(os.Stderr, "Validating against %s\n", planned)
		return
	}
	fmt.Fprintf(os.Stderr, "Validating against %s (Kubernetes %s)\n",
		planned, validate.NormalizeKubernetesVersion(flags.KubernetesVersion))
}

// checkValidateFlags rejects malformed flag combinations up front: an
// unknown --output, a short Kubernetes version (a form like "1.36" would
// 404 every default-registry schema and silently skip all native kinds,
// looking like a green run) and a negative download timeout.
func checkValidateFlags(flags *ValidateFlags) error {
	switch flags.Output {
	case "", "text", "json", "junit":
	default:
		return NewExitError(fmt.Errorf("unknown --output format %q (use text, json or junit)", flags.Output), ExitCodeError)
	}
	if err := validate.ValidateKubernetesVersion(flags.KubernetesVersion); err != nil {
		return NewExitError(err, ExitCodeError)
	}
	if flags.SchemaDownloadTimeout < 0 {
		return NewExitError(fmt.Errorf("invalid --schema-download-timeout %s: must be 0 (no limit) or a positive duration", flags.SchemaDownloadTimeout), ExitCodeError)
	}
	return nil
}

// resolveValidatePaths resolves and sanity-checks the cluster path: it
// must exist and contain Kustomization files directly (not just in
// subdirectories). Returns the absolute cluster path and repo root.
func resolveValidatePaths(flags *ValidateFlags) (string, string, error) {
	clusterPath := flags.Path
	if clusterPath == "" {
		clusterPath = "."
	}

	absClusterPath, err := filepath.Abs(clusterPath)
	if err != nil {
		return "", "", NewExitError(fmt.Errorf("resolving path %s: %w", clusterPath, err), ExitCodeError)
	}

	if _, err := os.Stat(absClusterPath); os.IsNotExist(err) {
		return "", "", NewExitError(fmt.Errorf("path %s does not exist", clusterPath), ExitCodeError)
	}

	hasDirectKS, err := hasDirectKustomizations(absClusterPath)
	if err != nil {
		return "", "", NewExitError(fmt.Errorf("checking for Kustomization files: %w", err), ExitCodeError)
	}
	if !hasDirectKS {
		return "", "", NewExitError(fmt.Errorf("no Kustomization files found in %s", clusterPath), ExitCodeError)
	}

	repoRoot, err := git.FindRepoRoot(absClusterPath)
	if err != nil {
		return "", "", NewExitError(fmt.Errorf("finding git repo root: %w", err), ExitCodeError)
	}
	return absClusterPath, repoRoot, nil
}

// resolveValidateCaches resolves the two validate-side schema caches; the
// disable word turns each into a per-run temp directory. The returned
// cleanup closes both (kubeconform reads schemas lazily — call it only
// after validation).
func resolveValidateCaches(flags *ValidateFlags) (schemaCacheDir, crdCacheRoot string, cleanup func(), err error) {
	schemaCacheDir, cleanupSchemas, err := validate.CacheDirOrTemp(flags.schemaCacheDir(), "fluxview-schemas-")
	if err != nil {
		return "", "", nil, NewExitError(err, ExitCodeError)
	}
	crdCacheRoot, cleanupCRDCache, err := validate.CacheDirOrTemp(flags.crdSchemaCacheDir(), "fluxview-crd-schemas-")
	if err != nil {
		cleanupSchemas()
		return "", "", nil, NewExitError(err, ExitCodeError)
	}
	return schemaCacheDir, crdCacheRoot, func() {
		cleanupCRDCache()
		cleanupSchemas()
	}, nil
}

// validateBuildState carries what runValidate needs to inspect one build:
// the build result cache, the anomaly report and the git source env
// (closed after validation — the clones back external sources the build
// already consumed).
type validateBuildState struct {
	cache  buildCache
	report buildReport
	close  func()
}

// buildValidationOutput builds the Flux Kustomization pipeline output the
// gate validates. Errors are already exit-coded by the build pipeline.
func buildValidationOutput(ctx context.Context, flags *ValidateFlags, absClusterPath, repoRoot string) ([]byte, *validateBuildState, error) {
	scans := newScanCache()
	kustomizations, err := scans.parserFor(absClusterPath).ParseKustomizations(ctx)
	if err != nil {
		return nil, nil, NewExitError(fmt.Errorf("parsing Kustomization resources: %w", err), ExitCodeError)
	}

	ksCache := flags.KsCache
	builder := kustomize.NewBuilder(repoRoot, ksCache.builderOptions()...)
	buildCache := make(buildCache)
	var report buildReport
	configMaps := resolveConfigMaps(ctx, scans, absClusterPath, builder, buildCache)
	secrets := resolveSecrets(ctx, scans, absClusterPath, builder, buildCache)

	gitEnv := newGitSourceEnv(ctx, repoRoot, ksCache, flags.NoGitSourceFetch)
	output, err := buildKSContent(ctx, &ksBuildEnv{
		scans:       scans,
		builder:     builder,
		repoRoot:    repoRoot,
		clusterPath: absClusterPath,
		cache:       buildCache,
		report:      &report,
		gitSources:  gitEnv,
	}, kustomizations, substitutionSources{configMaps: configMaps, secrets: secrets})
	if err != nil {
		gitEnv.Close()
		return nil, nil, NewExitError(err, ExitCodeError)
	}
	return output, &validateBuildState{cache: buildCache, report: report, close: func() { _ = gitEnv.Close() }}, nil
}

// gatePartialBuild turns build anomalies into gate failures: kustomize
// build failures and Kustomizations whose spec.path is missing from the
// repository — validating only the surviving subset would report success
// while resources are silently missing.
func gatePartialBuild(state *validateBuildState) error {
	if failed := state.cache.failedDirs(); len(failed) > 0 {
		return NewExitError(fmt.Errorf(
			"kustomize build failed for %d path(s), cannot validate: %s",
			len(failed), strings.Join(failed, ", ")), ExitCodeError)
	}

	if len(state.report.missingPaths) == 0 {
		return nil
	}
	parts := make([]string, len(state.report.missingPaths))
	for i, m := range state.report.missingPaths {
		parts[i] = m.ks + " (" + m.path + ")"
	}
	return NewExitError(fmt.Errorf(
		"%d Kustomization(s) point to a path missing from the repository, cannot validate: %s",
		len(parts), strings.Join(parts, ", ")), ExitCodeError)
}

// warnUncheckedKustomizations warns about external GitRepository sources
// that could not be fetched (an unreachable upstream or the
// --no-git-source-fetch kill switch); external content is best-effort for
// the gate. Causes are grouped (source + error) so a single global cause —
// e.g. the kill switch — is stated once with all its Kustomizations,
// instead of once per KS.
func warnUncheckedKustomizations(report buildReport) {
	if len(report.fetchErrors) == 0 {
		return
	}
	var order []string
	byCause := make(map[string][]string)
	for _, fe := range report.fetchErrors {
		cause := fe.source + ": " + fe.err
		if _, seen := byCause[cause]; !seen {
			order = append(order, cause)
		}
		byCause[cause] = append(byCause[cause], fe.ks)
	}
	parts := make([]string, 0, len(order))
	for _, cause := range order {
		parts = append(parts, strings.Join(byCause[cause], ", ")+" ("+cause+")")
	}
	fmt.Fprintf(os.Stderr,
		"Warning: %d Kustomization(s) left unchecked: %s\n",
		len(report.fetchErrors), strings.Join(parts, ", "))
}

// filterValidationOutput applies --namespace; done=true means there is
// nothing to validate and the message has already been printed.
func filterValidationOutput(flags *ValidateFlags, output []byte) (filtered []byte, done bool) {
	if flags.Namespace == "" {
		return output, false
	}
	filtered = filterByNamespace(output, flags.Namespace)
	if len(filtered) == 0 {
		fmt.Fprintf(os.Stderr, "No resources found in namespace %q\n", flags.Namespace)
		return nil, true
	}
	return filtered, false
}

// prefetchSchemas downloads the schemas the default registry would be
// asked for, under a cancellable client with per-request timeouts —
// kubeconform's HTTP loader has neither — so validation itself reads from
// the local cache and never waits on the network.
func prefetchSchemas(ctx context.Context, flags *ValidateFlags, output []byte, coverage validate.KindCoverage, schemaCacheDir string) error {
	if flags.disableDefaultSchemas {
		return nil
	}
	uncovered := validate.UncoveredKinds(
		validate.ResourceKinds(output), coverage, validate.NormalizeSkipKinds(flags.SkipKinds))

	// The default registry never carries Flux CRs (groups *.fluxcd.io):
	// fetching them is a guaranteed 404 and their absence says nothing
	// about --kubernetes-version — keep them out of the prefetch and of
	// the mass-skip warning below. A repo whose only uncovered kinds are
	// Flux CRs must not trip the warning when every custom kind is covered
	// by --schema-dir.
	fetchable := make([]validate.ResourceKind, 0, len(uncovered))
	for _, k := range uncovered {
		if !validate.IsFluxKind(k) {
			fetchable = append(fetchable, k)
		}
	}
	if len(fetchable) == 0 {
		return nil
	}

	missing, err := validate.PrefetchDefaultSchemas(ctx, fetchable, validate.PrefetchOptions{
		KubernetesVersion: flags.KubernetesVersion,
		Strict:            flags.Strict,
		CacheDir:          filepath.Join(schemaCacheDir, "registry"),
		BaseURL:           flags.testRegistryURL,
		// Flag semantics: 0 = no limit → the library's negative sentinel;
		// the flag default is a positive timeout.
		RequestTimeout: schemaDownloadTimeout(flags.SchemaDownloadTimeout),
	})
	if err != nil {
		if interrupted := CheckInterrupted(ctx); interrupted != nil {
			return interrupted
		}
		return NewExitError(fmt.Errorf("downloading Kubernetes schemas: %w", err), ExitCodeError)
	}
	// Every fetched kind 404'd: nothing that the registry should carry was
	// validated against. A published-but-wrong --kubernetes-version looks
	// exactly like this, so warn instead of a green-but-empty run.
	if len(missing) == len(fetchable) {
		fmt.Fprintf(os.Stderr,
			"Warning: the default registry has no schema for any of the %d kind(s) without a local schema (Kubernetes %s) — a wrong --kubernetes-version is a common cause; resources of these kinds are skipped\n",
			len(missing), validate.NormalizeKubernetesVersion(flags.KubernetesVersion))
	}
	return nil
}

// emitValidationReport writes the validation report: machine formats to
// stdout, human text to stderr (stdout stays reserved for machine
// output), and turns failures into the validation exit code.
func emitValidationReport(flags *ValidateFlags, results []validate.Result, failures []validate.Result) error {
	switch flags.Output {
	case "json":
		if err := writeValidationJSON(os.Stdout, results); err != nil {
			return NewExitError(fmt.Errorf("writing json output: %w", err), ExitCodeError)
		}
	case "junit":
		if err := writeValidationJUnit(os.Stdout, results); err != nil {
			return NewExitError(fmt.Errorf("writing junit output: %w", err), ExitCodeError)
		}
	default:
		if len(failures) == 0 {
			fmt.Fprintln(os.Stderr, "All resources valid.")
			return nil
		}
		writeValidationText(os.Stderr, failures)
	}

	if len(failures) > 0 {
		return NewExitError(fmt.Errorf("%d resource(s) failed validation", len(failures)), ExitValidationFailed)
	}
	return nil
}

// composeSchemaLocations builds the kubeconform schema location list: the
// schema directory first (JSON schemas, then CRD YAMLs converted to
// schemas), then any kubernetes-json-schema checkout found in or directly
// under it (native kinds offline), then the prefetched local copy of the
// default HTTP registry. All locations are local: runValidate prefetches
// the schemas the registry would be asked for under a bounded, cancellable
// client, so validation itself never waits on the network (kinds whose
// schema the registry does not carry are simply skipped there). The result
// is never nil — an empty non-nil list means "no sources" (tests disable
// the registry to stay offline) and must not trigger validate.New's
// default fallback. It also returns the local kind coverage, so the
// prefetch step knows which kinds to download. schemaCacheDir is the
// resolved downloaded-schema cache (its registry/ subdir is a location
// target); crdCacheRoot is the resolved CRD conversion cache — runValidate
// already mapped the disable word to a per-run temp directory, so a
// non-empty value is always passed here. A schemaDir that cannot be read
// yields an error, so typos fail the run instead of silently validating
// nothing.
func composeSchemaLocations(flags *ValidateFlags, schemaDir, schemaCacheDir, crdCacheRoot string) (locations []string, coverage validate.KindCoverage, err error) {
	locations = []string{}
	var flatDirs, versionedRoots []string

	if schemaDir != "" {
		locations = append(locations, filepath.Join(schemaDir, kubeconformLocalTemplate))
		flatDirs = append(flatDirs, schemaDir)
		// Converted CRDs persist in the CRD schema cache (keyed by source
		// size+mtime), so repeated runs skip reconversion.
		crdDir, cerr := validate.CRDYAMLToSchemaDir(schemaDir, crdCacheRoot)
		if cerr != nil {
			return nil, coverage, cerr
		}
		if crdDir != "" {
			locations = append(locations, filepath.Join(crdDir, kubeconformLocalTemplate))
			flatDirs = append(flatDirs, crdDir)
		}
		// A kubernetes-json-schema checkout in (or one level under) the
		// schema dir validates native kinds offline, ahead of the
		// prefetched registry copy.
		for _, root := range kubernetesJSONSchemaRoots(schemaDir) {
			locations = append(locations, filepath.Join(root, kubeconformVersionedTemplate))
			versionedRoots = append(versionedRoots, root)
		}
	}
	if !flags.disableDefaultSchemas {
		// The prefetched registry copy sits in the schema cache and is
		// populated by runValidate before validation runs.
		locations = append(locations, filepath.Join(schemaCacheDir, "registry", kubeconformVersionedTemplate))
	}

	coverage = validate.NewKindCoverage(flags.KubernetesVersion, flags.Strict, flatDirs, versionedRoots)
	return locations, coverage, nil
}

// plannedSchemaDisplay renders the schema sources known before the build:
// the schema directory and the default Kubernetes registry.
func plannedSchemaDisplay(flags *ValidateFlags, schemaDir string) string {
	var parts []string
	if schemaDir != "" {
		parts = append(parts, schemaDir)
	}
	if !flags.disableDefaultSchemas {
		parts = append(parts, "Kubernetes schemas")
	}
	return strings.Join(parts, ", ")
}

// kubernetesJSONSchemaRoots finds kubernetes-json-schema checkout roots in
// or directly under dir: the dir itself, or any immediate subdirectory,
// holding v<version>-standalone[-strict] subdirectories (the checkout
// layout of yannh/kubernetes-json-schema). Deeper nesting is not scanned —
// the dir (or its single subfolder) is where a checkout is unpacked.
func kubernetesJSONSchemaRoots(dir string) []string {
	candidates := []string{dir}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				candidates = append(candidates, filepath.Join(dir, e.Name()))
			}
		}
	}

	var roots []string
	for _, candidate := range candidates {
		if matches, err := filepath.Glob(filepath.Join(candidate, "*-standalone*")); err == nil && hasDir(matches) {
			roots = append(roots, candidate)
		}
	}
	return roots
}

// hasDir reports whether any of the paths is a directory.
func hasDir(paths []string) bool {
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// defaultSchemaDir returns the default schema directory: /schemas/
// (Docker) or ./schemas/ (local), or "" when neither exists.
func defaultSchemaDir() string {
	for _, dir := range []string{"/schemas", "schemas"} {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
	}
	return ""
}
