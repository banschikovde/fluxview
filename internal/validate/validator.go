// Package validate validates Kubernetes resources against JSON Schemas,
// delegating the schema sourcing and validation itself to the kubeconform
// library — the same engine and behavior as the kubeconform CLI.
//
// Schema sources are kubeconform schema locations in priority order: local
// directories with kubeconform-named JSON files (e.g. Flux crd-schemas.tar.gz
// contents or converted CRD YAMLs), HTTP URL templates, or the "default"
// location (Kubernetes schemas fetched from yannh/kubernetes-json-schema and
// cached on disk). Resources without a matching schema are skipped.
package validate

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	kcresource "github.com/yannh/kubeconform/pkg/resource"
	kcvalidator "github.com/yannh/kubeconform/pkg/validator"
)

// DefaultSchemaLocation is kubeconform's "default" location: Kubernetes
// schemas downloaded from yannh/kubernetes-json-schema for the configured
// Kubernetes version.
const DefaultSchemaLocation = "default"

// DefaultKubernetesVersion is the Kubernetes version used to resolve
// Kubernetes schemas when none is configured explicitly. Kept in sync with
// the k8s.io/* modules this project builds against.
const DefaultKubernetesVersion = "1.36.1"

// DefaultCacheBase returns the fluxview cache base: $XDG_CACHE_HOME/fluxview,
// else ~/.cache/fluxview. All cache dirs live under it, so one volume
// mount covers all of them.
func DefaultCacheBase() string {
	if base := os.Getenv("XDG_CACHE_HOME"); base != "" {
		return filepath.Join(base, "fluxview")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "fluxview-cache")
	}
	return filepath.Join(home, ".cache", "fluxview")
}

// DefaultSchemaCacheDir returns the schema cache directory:
// $XDG_CACHE_HOME/fluxview/schemas, else ~/.cache/fluxview/schemas.
// A sibling of the Helm and kustomize caches under the same parent, so one
// volume mount covers all of them. Downloaded schemas are version-pinned and
// never expire, so the cache is always on and has no flags.
func DefaultSchemaCacheDir() string {
	return filepath.Join(DefaultCacheBase(), "schemas")
}

// DefaultCRDSchemaCacheDir returns the cache directory for schemas converted
// from CRD YAML manifests: $XDG_CACHE_HOME/fluxview/crd-schemas, else
// ~/.cache/fluxview/crd-schemas. Conversion results are keyed by source
// size+mtime and reused across runs, so large CRD sets pay the conversion
// cost only when something changed.
func DefaultCRDSchemaCacheDir() string {
	return filepath.Join(DefaultCacheBase(), "crd-schemas")
}

// Options configures the Validator.
type Options struct {
	// SchemaLocations are kubeconform schema locations in priority order:
	// local directory paths, HTTP URL templates, or DefaultSchemaLocation.
	// The first location providing a schema for a resource wins. A nil list
	// falls back to DefaultSchemaLocation; an explicitly empty, non-nil
	// list means "no sources" — every resource is skipped.
	SchemaLocations []string
	// KubernetesVersion selects the Kubernetes schema version for
	// DefaultSchemaLocation (e.g. "1.36.1"). Defaults to
	// DefaultKubernetesVersion.
	KubernetesVersion string
	// CacheDir caches schemas downloaded over HTTP. Created when missing.
	// Empty disables the cache.
	CacheDir string
	// Strict rejects duplicated YAML keys in resources; for the default
	// HTTP registry it also switches to strict schemas that reject unknown
	// fields. Local schema directories are used as-is.
	Strict bool
	// SkipKinds skips validation of the listed kinds, in either form:
	// plain Kind ("Deployment", any apiVersion) or apiVersion/Kind
	// ("apps/v1/Deployment").
	SkipKinds []string
}

// Validator validates Kubernetes resources using kubeconform.
type Validator struct {
	v kcvalidator.Validator
}

// NormalizeKubernetesVersion strips whitespace and a leading "v" from a
// Kubernetes version ("v1.36.1" → "1.36.1"). An empty input stays empty.
func NormalizeKubernetesVersion(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "v")
}

// semverPattern matches the full X.Y.Z versions — the only per-release form
// kubernetes-json-schema publishes ("master" is the moving alias).
var semverPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// ValidateKubernetesVersion rejects versions that are not a full X.Y.Z (or
// "master") after normalization. A short form like "1.36" does not fail on
// its own: it makes every default-registry schema URL 404, so all native
// kinds are silently skipped and validation looks successful while nothing
// was checked. Failing here surfaces the typo before the build.
func ValidateKubernetesVersion(version string) error {
	normalized := NormalizeKubernetesVersion(version)
	if normalized == "" || normalized == "master" || semverPattern.MatchString(normalized) {
		return nil
	}
	return fmt.Errorf("invalid --kubernetes-version %q: want a full version like 1.36.1 (or master)", version)
}

// NormalizeSkipKinds builds the kubeconform skip set from raw flag values:
// both "Kind" (any apiVersion) and "apiVersion/Kind" entries, blank ones
// dropped.
func NormalizeSkipKinds(skipKinds []string) map[string]struct{} {
	set := make(map[string]struct{}, len(skipKinds))
	for _, kind := range skipKinds {
		if kind = strings.TrimSpace(kind); kind != "" {
			set[kind] = struct{}{}
		}
	}
	return set
}

// New creates a Validator from kubeconform schema locations.
func New(opts Options) (*Validator, error) {
	// A nil list means "not configured" — fall back to the default
	// registry. An explicitly empty, non-nil list means "no sources at
	// all": every resource is then skipped (used by tests to stay offline).
	// This must be handled here: kubeconform's New has its own fallback to
	// the default registry for empty lists.
	locations := opts.SchemaLocations
	if locations == nil {
		locations = []string{DefaultSchemaLocation}
	}
	if len(locations) == 0 {
		return &Validator{}, nil
	}

	kubernetesVersion := NormalizeKubernetesVersion(opts.KubernetesVersion)
	if kubernetesVersion == "" {
		kubernetesVersion = DefaultKubernetesVersion
	}

	if opts.CacheDir != "" {
		if err := os.MkdirAll(opts.CacheDir, 0755); err != nil {
			return nil, fmt.Errorf("creating schema cache dir %s: %w", opts.CacheDir, err)
		}
	}

	skipKinds := NormalizeSkipKinds(opts.SkipKinds)

	v, err := kcvalidator.New(locations, kcvalidator.Opts{
		Cache:                opts.CacheDir,
		KubernetesVersion:    kubernetesVersion,
		IgnoreMissingSchemas: true,
		Strict:               opts.Strict,
		SkipKinds:            skipKinds,
	})
	if err != nil {
		return nil, fmt.Errorf("creating kubeconform validator: %w", err)
	}

	return &Validator{v: v}, nil
}

// Status is the validation outcome for a single resource.
type Status string

const (
	// StatusValid: the resource conforms to its schema.
	StatusValid Status = "valid"
	// StatusSkipped: no schema matched the resource (or its kind is
	// skipped explicitly) — nothing was checked.
	StatusSkipped Status = "skipped"
	// StatusInvalid: the resource violates its schema.
	StatusInvalid Status = "invalid"
	// StatusError: validation could not run at all (unparseable resource,
	// unfetchable schema).
	StatusError Status = "error"
)

// Result holds the validation outcome for a single resource.
type Result struct {
	Kind      string
	Name      string
	Namespace string
	Status    Status
	Errors    []string
}

// Label renders "Kind namespace/name" for display; resources whose
// signature could not be parsed render as "malformed resource".
func (r Result) Label() string {
	if r.Kind == "" {
		return "malformed resource"
	}
	name := r.Name
	if r.Namespace != "" {
		name = r.Namespace + "/" + r.Name
	}
	return r.Kind + " " + name
}

// ValidateAll validates every document in multi-doc YAML and returns one
// Result per non-empty resource, including valid and skipped ones.
// Resources without a matching schema get StatusSkipped; resources whose
// schema cannot be fetched or parsed get StatusError. A Validator without
// schema sources skips everything.
func (v *Validator) ValidateAll(data []byte) []Result {
	if v.v == nil {
		return nil
	}

	var results []Result

	// FromStream's second channel is never written to in kubeconform
	// v0.8.0 (only closed), so there is nothing to drain.
	resources, _ := kcresource.FromStream(context.Background(), "input", bytes.NewReader(data))
	for res := range resources {
		r := v.v.ValidateResource(res)
		if r.Status == kcvalidator.Empty {
			continue
		}
		results = append(results, newResult(&res, r))
	}

	return results
}

// Validate returns only the failures: resources that are invalid or could
// not be validated at all.
func (v *Validator) Validate(data []byte) []Result {
	return Failures(v.ValidateAll(data))
}

// Failures filters results down to invalid and error entries.
func Failures(results []Result) []Result {
	var failed []Result
	for _, r := range results {
		if r.Status == StatusInvalid || r.Status == StatusError {
			failed = append(failed, r)
		}
	}
	return failed
}

// newResult maps a kubeconform result to a Result.
func newResult(res *kcresource.Resource, r kcvalidator.Result) Result {
	out := Result{}

	if sig, err := res.Signature(); err == nil && sig != nil && sig.Kind != "" {
		out.Kind, out.Name, out.Namespace = sig.Kind, sig.Name, sig.Namespace
	}

	switch r.Status {
	case kcvalidator.Valid:
		out.Status = StatusValid
	case kcvalidator.Skipped:
		out.Status = StatusSkipped
	case kcvalidator.Invalid:
		out.Status = StatusInvalid
		for _, ve := range r.ValidationErrors {
			if ve.Path != "" && ve.Path != "root" {
				out.Errors = append(out.Errors, fmt.Sprintf("%s: %s", ve.Path, ve.Msg))
			} else {
				out.Errors = append(out.Errors, ve.Msg)
			}
		}
	case kcvalidator.Error:
		out.Status = StatusError
		msg := "validation error"
		if r.Err != nil {
			msg = r.Err.Error()
		}
		out.Errors = []string{msg}
	}

	return out
}
