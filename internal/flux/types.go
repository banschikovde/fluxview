// Package flux provides types and parsing for Flux GitOps resources.
package flux

// Supported Flux API versions and kinds.
const (
	KindKustomization  = "Kustomization"
	KindHelmRelease    = "HelmRelease"
	KindHelmRepository = "HelmRepository"
	KindGitRepository  = "GitRepository"

	GroupSourceToolkitFluxHelmIO    = "source.toolkit.fluxcd.io"
	GroupKustomizeToolkitFluxHelmIO = "kustomize.toolkit.fluxcd.io"
	GroupHelmToolkitFluxHelmIO      = "helm.toolkit.fluxcd.io"

	VersionV1beta1 = "v1beta1"
	VersionV1beta2 = "v1beta2"
	VersionV1      = "v1"
	VersionV2beta1 = "v2beta1"
	VersionV2      = "v2"
)

// Kustomization represents a Flux Kustomization resource.
type Kustomization struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec KustomizationSpec `yaml:"spec"`
}

// KustomizationSpec holds the spec for a Flux Kustomization.
type KustomizationSpec struct {
	// SourceRef references a GitRepository or other source.
	SourceRef KustomizationSourceRef `yaml:"sourceRef"`
	// Path is the path within the source to run kustomize build on.
	Path string `yaml:"path,omitempty"`
	// Prune enables pruning.
	Prune bool `yaml:"prune,omitempty"`
	// Interval is the reconciliation interval.
	Interval struct {
		Duration string `yaml:"duration,omitempty"`
	} `yaml:"interval,omitempty"`
	// DependsOn lists dependencies.
	DependsOn []string `yaml:"dependsOn,omitempty"`
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
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec HelmReleaseSpec `yaml:"spec"`
}

// HelmReleaseSpec holds the spec for a Flux HelmRelease.
type HelmReleaseSpec struct {
	Chart    HelmReleaseChart `yaml:"chart"`
	Interval struct {
		Duration string `yaml:"duration,omitempty"`
	} `yaml:"interval,omitempty"`
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
	DependsOn []string `yaml:"dependsOn,omitempty"`
}

// HelmReleaseChart references the Helm chart to use.
type HelmReleaseChart struct {
	Spec HelmReleaseChartSpec `yaml:"spec"`
}

// HelmReleaseChartSpec specifies the chart source.
type HelmReleaseChartSpec struct {
	Chart    string `yaml:"chart"`
	Version  string `yaml:"version,omitempty"`
	SourceRef struct {
		Kind      string `yaml:"kind"`
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace,omitempty"`
	} `yaml:"sourceRef"`
	Interval *struct {
		Duration string `yaml:"duration,omitempty"`
	} `yaml:"interval,omitempty"`
	ReconcileStrategy string `yaml:"reconcileStrategy,omitempty"`
}

// HelmRepository represents a Flux HelmRepository resource.
type HelmRepository struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec HelmRepositorySpec `yaml:"spec"`
}

// HelmRepositorySpec holds the spec for a Flux HelmRepository.
type HelmRepositorySpec struct {
	URL      string `yaml:"url"`
	Interval struct {
		Duration string `yaml:"duration,omitempty"`
	} `yaml:"interval,omitempty"`
	SecretRef *struct {
		Name string `yaml:"name"`
	} `yaml:"secretRef,omitempty"`
	Type string `yaml:"type,omitempty"`
}

// GitRepository represents a Flux GitRepository resource.
type GitRepository struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec GitRepositorySpec `yaml:"spec"`
}

// GitRepositorySpec holds the spec for a Flux GitRepository.
type GitRepositorySpec struct {
	URL string `yaml:"url"`
	Ref struct {
		Branch string `yaml:"branch,omitempty"`
		Tag    string `yaml:"tag,omitempty"`
		Commit string `yaml:"commit,omitempty"`
	} `yaml:"ref"`
	Interval struct {
		Duration string `yaml:"duration,omitempty"`
	} `yaml:"interval,omitempty"`
	SecretRef *struct {
		Name string `yaml:"name"`
	} `yaml:"secretRef,omitempty"`
}

// Resource is a generic parsed Kubernetes/Flux resource with metadata.
type Resource struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
	// RawYAML is the original YAML bytes of this document.
	RawYAML []byte
}
