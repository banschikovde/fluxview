// Package flux provides types and parsing for Flux GitOps resources.
package flux

import (
	"encoding/base64"

	"github.com/banschikovde/fluxview/internal/kustomize"
)

// Supported Flux API versions and kinds.
const (
	KindKustomization  = "Kustomization"
	KindHelmRelease    = "HelmRelease"
	KindHelmRepository = "HelmRepository"
	KindOCIRepository  = "OCIRepository"
	KindGitRepository  = "GitRepository"
	KindBucket         = "Bucket"

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

func (k Kustomization) ident() string { return k.Metadata.Namespace + "/" + k.Metadata.Name }
func (k Kustomization) depKeys() []string {
	keys := make([]string, 0, len(k.Spec.DependsOn))
	for _, dep := range k.Spec.DependsOn {
		ns := dep.Namespace
		if ns == "" {
			ns = k.Metadata.Namespace
		}
		keys = append(keys, ns+"/"+dep.Name)
	}
	return keys
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
	Interval any `yaml:"interval,omitempty"`
	// RetryInterval is the retry interval (e.g. "1m").
	RetryInterval any `yaml:"retryInterval,omitempty"`
	// Timeout is the timeout for reconciliation.
	Timeout any `yaml:"timeout,omitempty"`
	// Wait enables waiting for health checks.
	Wait bool `yaml:"wait,omitempty"`
	// DependsOn lists dependencies.
	DependsOn []DependsOnEntry `yaml:"dependsOn,omitempty"`
	// PostBuild contains post-build substitutions.
	PostBuild *PostBuild `yaml:"postBuild,omitempty"`
	// Patches lists JSON6902 patches to apply to the build output.
	Patches []kustomize.PatchSpec `yaml:"patches,omitempty"`
	// TargetNamespace sets metadata.namespace on all namespaced resources in
	// the build output, matching Flux's Kustomization.spec.targetNamespace.
	// Cluster-scoped resources are left untouched.
	TargetNamespace string `yaml:"targetNamespace,omitempty"`
	// Images lists image overrides (kustomize image transformer), applied to
	// the build output to rewrite container image references.
	Images []kustomize.ImageOverride `yaml:"images,omitempty"`
	// Suspend indicates if the resource is suspended.
	Suspend bool `yaml:"suspend,omitempty"`
	// HealthChecks lists health check references.
	HealthChecks any `yaml:"healthChecks,omitempty"`
	// Decryption configures SOPS decryption.
	Decryption any `yaml:"decryption,omitempty"`
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
	SubstituteFrom    any               `yaml:"substituteFrom,omitempty"`
	AllowInsecure     bool              `yaml:"allowInsecure,omitempty"`
	DisableSubstitute bool              `yaml:"disableSubstitute,omitempty"`
}

// KustomizationSourceRef references the source for a Kustomization.
type KustomizationSourceRef struct {
	APIVersion string `yaml:"apiVersion,omitempty"`
	Kind       string `yaml:"kind,omitempty"`
	Name       string `yaml:"name,omitempty"`
	Namespace  string `yaml:"namespace,omitempty"`
}

// HelmRelease represents a Flux HelmRelease resource.
type HelmRelease struct {
	APIVersion string          `yaml:"apiVersion"`
	Kind       string          `yaml:"kind"`
	Metadata   ObjectMeta      `yaml:"metadata"`
	Spec       HelmReleaseSpec `yaml:"spec"`
}

func (h HelmRelease) ident() string { return h.Metadata.Namespace + "/" + h.Metadata.Name }
func (h HelmRelease) depKeys() []string {
	keys := make([]string, 0, len(h.Spec.DependsOn))
	for _, dep := range h.Spec.DependsOn {
		ns := dep.Namespace
		if ns == "" {
			ns = h.Metadata.Namespace
		}
		keys = append(keys, ns+"/"+dep.Name)
	}
	return keys
}

// HelmReleaseSpec holds the spec for a Flux HelmRelease.
type HelmReleaseSpec struct {
	Chart HelmReleaseChart `yaml:"chart"`
	// ChartRef references an OCIRepository directly (Flux v2 chartRef API).
	ChartRef *ChartRef `yaml:"chartRef,omitempty"`
	Interval any       `yaml:"interval,omitempty"`
	// Values holds the values to pass to helm template.
	Values map[string]any `yaml:"values,omitempty"`
	// ValuesFrom references ConfigMaps/Secrets with values.
	ValuesFrom any `yaml:"valuesFrom,omitempty"`
	// Suspend indicates if the resource is suspended.
	Suspend bool `yaml:"suspend,omitempty"`
	// TargetNamespace overrides the release namespace.
	TargetNamespace string `yaml:"targetNamespace,omitempty"`
	// ReleaseName overrides the Helm release name (defaults to metadata.name).
	// Affects {{ .Release.Name }} in chart templates.
	ReleaseName string `yaml:"releaseName,omitempty"`
	// Install holds install-specific configuration.
	Install *InstallSpec `yaml:"install,omitempty"`
	// Upgrade holds upgrade-specific configuration.
	Upgrade *UpgradeSpec `yaml:"upgrade,omitempty"`
	// DependsOn lists dependencies.
	DependsOn []DependsOnEntry `yaml:"dependsOn,omitempty"`
	// PostRenderers lists post-renderers applied after helm template.
	PostRenderers []PostRenderer `yaml:"postRenderers,omitempty"`
}

// InstallSpec holds install-specific configuration for a HelmRelease.
// Only CRDs is relevant to rendering: "Skip" excludes the chart's CRDs from the
// rendered output (Flux values: Skip/Create/CreateReplace).
type InstallSpec struct {
	CRDs string `yaml:"crds,omitempty"`
}

// UpgradeSpec holds upgrade-specific configuration. CRDs is parsed for API
// completeness but intentionally unused: InflateHelmRelease always renders via
// action.NewInstall (it cannot know whether the release is already installed in
// a real cluster), so spec.upgrade.crds is irrelevant for this rendering pipeline.
type UpgradeSpec struct {
	CRDs string `yaml:"crds,omitempty"`
}

// PostRenderer represents a single postRenderer entry.
type PostRenderer struct {
	Kustomize *PostRendererKustomize `yaml:"kustomize,omitempty"`
}

// PostRendererKustomize holds kustomize patches for post-rendering.
type PostRendererKustomize struct {
	Patches []kustomize.PatchSpec `yaml:"patches,omitempty"`
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
	Interval any               `yaml:"interval,omitempty"`
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
	// ValuesFiles lists extra values files inside the chart itself, merged over
	// the chart's values.yaml in order (Flux chart.spec.valuesFiles).
	ValuesFiles       []string `yaml:"valuesFiles,omitempty"`
	Interval          any      `yaml:"interval,omitempty"`
	ReconcileStrategy string   `yaml:"reconcileStrategy,omitempty"`
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
	URL       string `yaml:"url"`
	Interval  any    `yaml:"interval,omitempty"`
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

// Secret represents a Kubernetes Secret resource.
type Secret struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   ObjectMeta        `yaml:"metadata"`
	Data       map[string]string `yaml:"data,omitempty"`
	StringData map[string]string `yaml:"stringData,omitempty"`
}

// ValuesFromEntry represents a single entry in the valuesFrom list.
type ValuesFromEntry struct {
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace,omitempty"`
	ValuesKey string `yaml:"valuesKey,omitempty"`
	Optional  bool   `yaml:"optional,omitempty"`
}

// GetSecretValue returns the decoded value from a Secret.
// It tries stringData first (plain text), then falls back to data (base64-encoded).
// Returns empty string if value not found or decoding fails.
func (s *Secret) GetSecretValue(key string) string {
	if s.StringData != nil {
		if value, ok := s.StringData[key]; ok {
			return value
		}
	}
	if s.Data != nil {
		if value, ok := s.Data[key]; ok {
			decoded, err := base64.StdEncoding.DecodeString(value)
			if err == nil {
				return string(decoded)
			}
		}
	}
	return ""
}
