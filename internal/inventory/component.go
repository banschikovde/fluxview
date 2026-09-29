// Package inventory builds the software inventory of a cluster from the
// Flux GitOps repository: one Component per deployed software unit, collected
// from HelmReleases, plain manifests (Kustomization workloads), operator
// custom resources and CRDs, then rendered as a table or Markdown.
package inventory

// Source kinds of a Component (the SOURCE column).
const (
	SourceHelm     = "helm"     // HelmRelease
	SourceManifest = "manifest" // workload from plain k8s manifests
	SourceCR       = "cr"       // operator-managed custom resource
	SourceCRD      = "crd"      // CRD group installed as a separate artifact
)

// VersionSource values: where a component's version came from.
const (
	VersionAppVersion = "appVersion" // Chart.yaml appVersion
	VersionLabel      = "label"      // app.kubernetes.io/version label
	VersionImage      = "image"      // container image tag
	VersionCRField    = "cr-field"   // version field of a custom resource
	VersionDefault    = "default"    // operator default (not set in the CR)
	VersionGitRef     = "git-ref"    // pinned tag of the GitRepository that ships the CRDs
)

// DefaultVersion is the version shown for a CR whose rule matched but no
// version field was set: the operator's built-in default.
const DefaultVersion = "default"

// UnknownVersion is the version shown for rule-less CRs under --all-crs.
const UnknownVersion = "unknown"

// CanonicalSources is the display order of sources in the summary line.
var CanonicalSources = []string{SourceHelm, SourceCR, SourceManifest, SourceCRD}

// Component is one software unit in the inventory output — one table row.
// The table and markdown formats are two projections of it; the struct is
// not a public contract.
type Component struct {
	Source        string     `json:"source"` // helm | manifest | cr | crd
	Kind          string     `json:"kind"`   // HelmRelease, Deployment, VMCluster...
	Namespace     string     `json:"namespace"`
	Name          string     `json:"name"`
	Software      string     `json:"software"`
	Version       string     `json:"version"`       // appVersion / image tag / version from CR
	VersionSource string     `json:"versionSource"` // appVersion | label | image | cr-field | default
	Chart         *ChartInfo `json:"chart,omitempty"`
	Operator      string     `json:"operator,omitempty"`
	Images        []string   `json:"images,omitempty"`
	// Parent identifies the HelmRelease that rendered this component as
	// "namespace/name" — set for chart-rendered CRs and umbrella children,
	// empty for components from plain manifests. Namespace-qualified so
	// same-named releases in different namespaces never collide.
	Parent   string   `json:"parent,omitempty"`
	FluxKs   string   `json:"fluxKustomization"`
	Path     string   `json:"path"`
	Warnings []string `json:"warnings,omitempty"`
	// Change carries the --branch-orig delta ("+", "-" or "~ old → new");
	// empty on a component without changes. Not part of the stable shape.
	Change string `json:"-"`
}

// ChartInfo describes the Helm chart behind a component.
type ChartInfo struct {
	Name       string `json:"name"`
	Version    string `json:"version"`    // resolved (actual) chart version
	Constraint string `json:"constraint"` // original spec version when it was a range
	SourceKind string `json:"sourceKind"` // HelmRepository | OCIRepository | GitRepository | Bucket
	SourceURL  string `json:"sourceURL"`
}
