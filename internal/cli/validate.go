package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

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
	RemoteCacheDir        string
	RemoteCacheTTL        time.Duration
	RemoteCacheTimeout    time.Duration
	BuildCacheDir         string
	BuildCacheTTL         time.Duration
	GitSourceCacheDir     string
	GitSourceCacheTTL     time.Duration

	// disableDefaultSchemas drops the default HTTP schema registry. It has
	// no flag — only tests set it, to keep them offline.
	disableDefaultSchemas bool
	// testRegistryURL points the schema prefetch at a test HTTP server;
	// testCacheBase overrides the fluxview cache base (schemas, CRD
	// conversion). No flags — tests only.
	testRegistryURL string
	testCacheBase   string
}

// cacheBase is the root for validate's on-disk caches (downloaded schemas,
// converted CRDs); tests override it to stay hermetic.
func (f *ValidateFlags) cacheBase() string {
	if f.testCacheBase != "" {
		return f.testCacheBase
	}
	return validate.DefaultCacheBase()
}

// registryCacheDir caches schemas prefetched from the default registry, in
// the kubernetes-json-schema layout.
func (f *ValidateFlags) registryCacheDir() string {
	return filepath.Join(f.cacheBase(), "schemas", "registry")
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
	registerKustomizeCacheFlags(cmd, &flags.RemoteCacheDir, &flags.RemoteCacheTTL, &flags.RemoteCacheTimeout, &flags.BuildCacheDir, &flags.BuildCacheTTL, &flags.GitSourceCacheDir, &flags.GitSourceCacheTTL)

	return cmd
}

func runValidate(ctx context.Context, flags *ValidateFlags) error {
	switch flags.Output {
	case "", "text", "json", "junit":
	default:
		return NewExitError(fmt.Errorf("unknown --output format %q (use text, json or junit)", flags.Output), ExitCodeError)
	}

	// Reject a malformed version before the build: a short form like "1.36"
	// would 404 every default-registry schema and silently skip all native
	// kinds, looking like a green run.
	if err := validate.ValidateKubernetesVersion(flags.KubernetesVersion); err != nil {
		return NewExitError(err, ExitCodeError)
	}

	if flags.SchemaDownloadTimeout < 0 {
		return NewExitError(fmt.Errorf("invalid --schema-download-timeout %s: must be 0 (no limit) or a positive duration", flags.SchemaDownloadTimeout), ExitCodeError)
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

	// Check that the path contains Kustomization files directly (not just in subdirectories)
	hasDirectKS, err := hasDirectKustomizations(absClusterPath)
	if err != nil {
		return NewExitError(fmt.Errorf("checking for Kustomization files: %w", err), ExitCodeError)
	}
	if !hasDirectKS {
		return NewExitError(fmt.Errorf("no Kustomization files found in %s", clusterPath), ExitCodeError)
	}

	repoRoot, err := git.FindRepoRoot(absClusterPath)
	if err != nil {
		return NewExitError(fmt.Errorf("finding git repo root: %w", err), ExitCodeError)
	}

	// Resolve the schema directory.
	schemaDir := flags.SchemaDir
	if schemaDir == "" {
		schemaDir = defaultSchemaDir()
	}

	// Announce the validation context before the (possibly slow) build, so
	// it is clear what resources will be validated against while it runs.
	if planned := plannedSchemaDisplay(flags, schemaDir); planned != "" {
		if flags.disableDefaultSchemas {
			fmt.Fprintf(os.Stderr, "Validating against %s\n", planned)
		} else {
			fmt.Fprintf(os.Stderr, "Validating against %s (Kubernetes %s)\n",
				planned, validate.NormalizeKubernetesVersion(flags.KubernetesVersion))
		}
	}

	scans := newScanCache()
	parser := scans.parserFor(absClusterPath)
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
	var report buildReport
	configMaps := resolveConfigMaps(ctx, scans, absClusterPath, builder, buildCache)
	secrets := resolveSecrets(ctx, scans, absClusterPath, builder, buildCache)

	gitEnv := newGitSourceEnv(ctx, repoRoot, ksCache)
	defer gitEnv.Close()
	output, err := buildKSContent(ctx, scans, builder, kustomizations, repoRoot, absClusterPath, configMaps, secrets, false, buildCache, &report, gitEnv)
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}

	// A validation gate must not pass on a partial build: kustomize build
	// failures (e.g. malformed YAML) are warn-and-continue for build and
	// diff, but validating only the surviving subset would report success
	// while resources are silently missing.
	if failed := buildCache.failedDirs(); len(failed) > 0 {
		return NewExitError(fmt.Errorf(
			"kustomize build failed for %d path(s), cannot validate: %s",
			len(failed), strings.Join(failed, ", ")), ExitCodeError)
	}

	// Same for Flux Kustomizations pointing outside the repository (or at
	// a nonexistent path): their resources never reached the checked set.
	if len(report.missingPaths) > 0 {
		parts := make([]string, len(report.missingPaths))
		for i, m := range report.missingPaths {
			parts[i] = m.ks + " (" + m.path + ")"
		}
		return NewExitError(fmt.Errorf(
			"%d Kustomization(s) point to a path missing from the repository, cannot validate: %s",
			len(parts), strings.Join(parts, ", ")), ExitCodeError)
	}

	// And for external GitRepository sources that could not be fetched:
	// validating the surviving subset would report success while the
	// external resources went unchecked.
	if len(report.fetchErrors) > 0 {
		parts := make([]string, len(report.fetchErrors))
		for i, fe := range report.fetchErrors {
			parts[i] = fe.ks + " (" + fe.source + ": " + fe.err + ")"
		}
		return NewExitError(fmt.Errorf(
			"%d Kustomization(s) failed to fetch their external source, cannot validate: %s",
			len(parts), strings.Join(parts, ", ")), ExitCodeError)
	}

	if output == nil {
		fmt.Fprintln(os.Stderr, "No resources to validate.")
		return nil
	}

	// Check for interruption before validation
	if err := CheckInterrupted(ctx); err != nil {
		return err
	}

	// CRDs are schema definitions, not resources to validate — drop them.
	output = filterCRDDocs(output)

	if flags.Namespace != "" {
		output = filterByNamespace(output, flags.Namespace)
		if len(output) == 0 {
			fmt.Fprintf(os.Stderr, "No resources found in namespace %q\n", flags.Namespace)
			return nil
		}
	}

	locations, coverage, err := composeSchemaLocations(flags, schemaDir)
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}

	// kubeconform's HTTP loader has no timeout and takes no context, so a
	// hung registry would hang validation forever. Instead, prefetch the
	// schemas it would request under a cancellable client with per-request
	// timeouts; validation then reads them from the local cache.
	if !flags.disableDefaultSchemas {
		uncovered := validate.UncoveredKinds(
			validate.ResourceKinds(output), coverage, validate.NormalizeSkipKinds(flags.SkipKinds))

		// The default registry never carries Flux CRs (groups
		// *.fluxcd.io): fetching them is a guaranteed 404 and their
		// absence says nothing about --kubernetes-version — keep them out
		// of the prefetch and of the mass-skip warning below. A repo whose
		// only uncovered kinds are Flux CRs must not trip the warning when
		// every custom kind is covered by --schema-dir.
		fetchable := make([]validate.ResourceKind, 0, len(uncovered))
		for _, k := range uncovered {
			if !validate.IsFluxKind(k) {
				fetchable = append(fetchable, k)
			}
		}

		if len(fetchable) > 0 {
			missing, err := validate.PrefetchDefaultSchemas(ctx, fetchable, validate.PrefetchOptions{
				KubernetesVersion: flags.KubernetesVersion,
				Strict:            flags.Strict,
				CacheDir:          flags.registryCacheDir(),
				BaseURL:           flags.testRegistryURL,
				// Flag semantics: 0 = no limit → the library's negative
				// sentinel; the flag default is a positive timeout.
				RequestTimeout: schemaDownloadTimeout(flags.SchemaDownloadTimeout),
			})
			if err != nil {
				if interrupted := CheckInterrupted(ctx); interrupted != nil {
					return interrupted
				}
				return NewExitError(fmt.Errorf("downloading Kubernetes schemas: %w", err), ExitCodeError)
			}
			// Every fetched kind 404'd: nothing that the registry should
			// carry was validated against. A published-but-wrong
			// --kubernetes-version looks exactly like this, so warn instead
			// of a green-but-empty run.
			if len(missing) == len(fetchable) {
				fmt.Fprintf(os.Stderr,
					"Warning: the default registry has no schema for any of the %d kind(s) without a local schema (Kubernetes %s) — a wrong --kubernetes-version is a common cause; resources of these kinds are skipped\n",
					len(missing), validate.NormalizeKubernetesVersion(flags.KubernetesVersion))
			}
		}
	}

	validator, err := validate.New(validate.Options{
		SchemaLocations:   locations,
		KubernetesVersion: flags.KubernetesVersion,
		// Downloaded schemas are version-pinned and immutable — cache them
		// unconditionally; the cache only saves network round-trips.
		CacheDir:  filepath.Join(flags.cacheBase(), "schemas"),
		Strict:    flags.Strict,
		SkipKinds: flags.SkipKinds,
	})
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}

	results := validator.ValidateAll(output)
	failures := validate.Failures(results)

	// Machine formats go to stdout, human text to stderr (stdout stays
	// reserved for machine output).
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
// prefetch step knows which kinds to download. A schemaDir that cannot be
// read yields an error, so typos fail the run instead of silently
// validating nothing.
func composeSchemaLocations(flags *ValidateFlags, schemaDir string) (locations []string, coverage validate.KindCoverage, err error) {
	locations = []string{}
	var flatDirs, versionedRoots []string

	if schemaDir != "" {
		locations = append(locations, filepath.Join(schemaDir, kubeconformLocalTemplate))
		flatDirs = append(flatDirs, schemaDir)
		// Converted CRDs persist in the CRD schema cache (keyed by source
		// size+mtime), so repeated runs skip reconversion.
		crdCache := filepath.Join(flags.cacheBase(), "crd-schemas")
		crdDir, cerr := validate.CRDYAMLToSchemaDir(schemaDir, crdCache)
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
		locations = append(locations, filepath.Join(flags.registryCacheDir(), kubeconformVersionedTemplate))
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
