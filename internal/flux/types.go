// Package flux provides types and parsing for Flux GitOps resources.
package flux

// Supported Flux API versions and kinds.
const (
	KindKustomization  = "Kustomization"
	KindHelmRelease    = "HelmRelease"
	KindHelmRepository = "HelmRepository"
	KindOCIRepository  = "OCIRepository"

	GroupSourceToolkitFluxHelmIO    = "source.toolkit.fluxcd.io"
	GroupKustomizeToolkitFluxHelmIO = "kustomize.toolkit.fluxcd.io"
	GroupHelmToolkitFluxHelmIO      = "helm.toolkit.fluxcd.io"
)

// ObjectMeta holds the standard Kubernetes metadata fields used across all
// Flux resource types. Named type to avoid repeating anonymous structs.
type ObjectMeta struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// Kustomization represents a Flux Kustomization resource.
type Kustomization struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   ObjectMeta        `yaml:"metadata"`
	Spec       KustomizationSpec `yaml:"spec"`
}

// KustomizationSpec holds the spec for a Flux Kustomization.
type KustomizationSpec struct {
	// SourceRef references a GitRepository or other source.
	SourceRef KustomizationSourceRef `yaml:"sourceRef"`
	// Path is the path within the source to run kustomize build on.
	Path string `yaml:"path,omitempty"`
	// Prune enables pruning.
	Prune bool `yaml:"prune,omitempty"`
	// Interval is the reconciliation interval (e.g. "5m").
	Interval interface{} `yaml:"interval,omitempty"`
	// RetryInterval is the retry interval (e.g. "1m").
	RetryInterval interface{} `yaml:"retryInterval,omitempty"`
	// Timeout is the timeout for reconciliation.
	Timeout interface{} `yaml:"timeout,omitempty"`
	// Wait enables waiting for health checks.
	Wait bool `yaml:"wait,omitempty"`
	// DependsOn lists dependencies.
	DependsOn []DependsOnEntry `yaml:"dependsOn,omitempty"`
	// PostBuild contains post-build substitutions.
	PostBuild *PostBuild `yaml:"postBuild,omitempty"`
	// Patches lists patches to apply.
	Patches interface{} `yaml:"patches,omitempty"`
	// Images lists image overrides.
	Images interface{} `yaml:"images,omitempty"`
	// Suspend indicates if the resource is suspended.
	Suspend bool `yaml:"suspend,omitempty"`
	// HealthChecks lists health check references.
	HealthChecks interface{} `yaml:"healthChecks,omitempty"`
	// Decryption configures SOPS decryption.
	Decryption interface{} `yaml:"decryption,omitempty"`
	// Force enables force reconciliation.
	Force bool `yaml:"force,omitempty"`
}

// DependsOnEntry represents a dependency reference.
type DependsOnEntry struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace,omitempty"`
}

// PostBuild contains post-build substitution configuration.
type PostBuild struct {
	Substitute        map[string]string `yaml:"substitute,omitempty"`
	SubstituteFrom    interface{}       `yaml:"substituteFrom,omitempty"`
	AllowInsecure     bool              `yaml:"allowInsecure,omitempty"`
	DisableSubstitute bool              `yaml:"disableSubstitute,omitempty"`
}

// KustomizationSourceRef references the source for a Kustomization.
type KustomizationSourceRef struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Name       string `yaml:"name"`
	Namespace  string `yaml:"namespace,omitempty"`
}

// HelmRelease represents a Flux HelmRelease resource.
type HelmRelease struct {
	APIVersion string          `yaml:"apiVersion"`
	Kind       string          `yaml:"kind"`
	Metadata   ObjectMeta      `yaml:"metadata"`
	Spec       HelmReleaseSpec `yaml:"spec"`
}

// HelmReleaseSpec holds the spec for a Flux HelmRelease.
type HelmReleaseSpec struct {
	Chart HelmReleaseChart `yaml:"chart"`
	// ChartRef references an OCIRepository directly (Flux v2 chartRef API).
	ChartRef *ChartRef   `yaml:"chartRef,omitempty"`
	Interval interface{} `yaml:"interval,omitempty"`
	// Values holds the values to pass to helm template.
	Values map[string]interface{} `yaml:"values,omitempty"`
	// ValuesFrom references ConfigMaps/Secrets with values.
	ValuesFrom interface{} `yaml:"valuesFrom,omitempty"`
	// Suspend indicates if the resource is suspended.
	Suspend bool `yaml:"suspend,omitempty"`
	// TargetNamespace overrides the release namespace.
	TargetNamespace string `yaml:"targetNamespace,omitempty"`
	// Install holds install-specific configuration.
	Install interface{} `yaml:"install,omitempty"`
	// Upgrade holds upgrade-specific configuration.
	Upgrade interface{} `yaml:"upgrade,omitempty"`
	// DependsOn lists dependencies.
	DependsOn []DependsOnEntry `yaml:"dependsOn,omitempty"`
}

// HelmReleaseChart references the Helm chart to use.
type HelmReleaseChart struct {
	Spec HelmReleaseChartSpec `yaml:"spec"`
}

// ChartRef references a source (e.g. OCIRepository) for Flux v2 chartRef API.
type ChartRef struct {
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace,omitempty"`
}

// OCIRepository represents a Flux OCIRepository resource.
type OCIRepository struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   ObjectMeta        `yaml:"metadata"`
	Spec       OCIRepositorySpec `yaml:"spec"`
}

// OCIRepositorySpec holds the spec for a Flux OCIRepository.
type OCIRepositorySpec struct {
	URL      string            `yaml:"url"`
	Ref      *OCIRepositoryRef `yaml:"ref,omitempty"`
	Interval interface{}       `yaml:"interval,omitempty"`
}

// OCIRepositoryRef specifies which artifact version to pull.
// Priority: digest > semver > tag > latest.
type OCIRepositoryRef struct {
	Tag    string `yaml:"tag,omitempty"`
	Semver string `yaml:"semver,omitempty"`
	Digest string `yaml:"digest,omitempty"`
}

// ResolveVersion returns the chart version to use for this OCIRepository.
// Priority: digest > semver > tag > empty (latest). Matches Flux source-controller.
// Note: digest is NOT returned here — it's handled separately by appending
// @sha256:... to the OCI URL.
func (r *OCIRepositoryRef) ResolveVersion() string {
	if r == nil {
		return ""
	}
	if r.Semver != "" {
		return r.Semver
	}
	if r.Tag != "" {
		return r.Tag
	}
	return ""
}

// HasDigest returns true if a digest reference is set.
func (r *OCIRepositoryRef) HasDigest() bool {
	return r != nil && r.Digest != ""
}

// HelmReleaseChartSpec specifies the chart source.
type HelmReleaseChartSpec struct {
	Chart     string `yaml:"chart"`
	Version   string `yaml:"version,omitempty"`
	SourceRef struct {
		Kind      string `yaml:"kind"`
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace,omitempty"`
	} `yaml:"sourceRef"`
	Interval          interface{} `yaml:"interval,omitempty"`
	ReconcileStrategy string      `yaml:"reconcileStrategy,omitempty"`
}

// HelmRepository represents a Flux HelmRepository resource.
type HelmRepository struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	Metadata   ObjectMeta         `yaml:"metadata"`
	Spec       HelmRepositorySpec `yaml:"spec"`
}

// HelmRepositorySpec holds the spec for a Flux HelmRepository.
type HelmRepositorySpec struct {
	URL       string      `yaml:"url"`
	Interval  interface{} `yaml:"interval,omitempty"`
	SecretRef *struct {
		Name string `yaml:"name"`
	} `yaml:"secretRef,omitempty"`
	Type string `yaml:"type,omitempty"`
}

// ConfigMap represents a Kubernetes ConfigMap resource.
type ConfigMap struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   ObjectMeta        `yaml:"metadata"`
	Data       map[string]string `yaml:"data,omitempty"`
}
