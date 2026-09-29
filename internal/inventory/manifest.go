package inventory

import (
	"fmt"
	"sort"
	"strings"
)

// workloadKinds are the kinds grouped into manifest components, in
// version-priority order: when several workloads share a component name,
// the earliest kind here supplies the version.
var workloadKinds = []string{"Deployment", "StatefulSet", "DaemonSet", "CronJob", "Job"}

// knownSidecars are injected proxies never chosen as the main container.
var knownSidecars = map[string]bool{
	"istio-proxy":   true,
	"linkerd-proxy": true,
	"envoy":         true,
}

// workloadKindRank orders workload kinds for deterministic grouping.
func workloadKindRank(kind string) int {
	for i, k := range workloadKinds {
		if k == kind {
			return i
		}
	}
	return len(workloadKinds)
}

// IsWorkload reports whether kind is grouped into manifest components.
func IsWorkload(kind string) bool {
	return workloadKindRank(kind) < len(workloadKinds)
}

// workloadDoc is one parsed workload document.
type workloadDoc struct {
	kind      string
	name      string
	namespace string
	labels    map[string]string
	// podSpec is the PodSpec-level mapping holding containers.
	podSpec map[string]interface{}
}

// componentName resolves the component name by priority:
// app.kubernetes.io/name → app.kubernetes.io/part-of → workload name.
func (w workloadDoc) componentName() string {
	if v := w.labels["app.kubernetes.io/name"]; v != "" {
		return v
	}
	if v := w.labels["app.kubernetes.io/part-of"]; v != "" {
		return v
	}
	return w.name
}

// parseWorkloadDoc extracts the workload-relevant fields from a built
// document. ok is false for non-workload or malformed documents.
func parseWorkloadDoc(raw map[string]interface{}) (workloadDoc, bool) {
	kind, _ := raw["kind"].(string)
	if !IsWorkload(kind) {
		return workloadDoc{}, false
	}
	metadata, _ := raw["metadata"].(map[string]interface{})
	if metadata == nil {
		return workloadDoc{}, false
	}
	name, _ := metadata["name"].(string)
	if name == "" {
		return workloadDoc{}, false
	}
	namespace, _ := metadata["namespace"].(string)
	labels := map[string]string{}
	if lm, ok := metadata["labels"].(map[string]interface{}); ok {
		for k, v := range lm {
			if s, ok := v.(string); ok {
				labels[k] = s
			}
		}
	}

	spec, _ := raw["spec"].(map[string]interface{})
	if spec == nil {
		return workloadDoc{}, false
	}
	// CronJob nests the pod template one level deeper.
	if kind == "CronJob" {
		jobTemplate, _ := spec["jobTemplate"].(map[string]interface{})
		if jobTemplate != nil {
			if jtSpec, ok := jobTemplate["spec"].(map[string]interface{}); ok {
				spec = jtSpec
			}
		}
	}
	podSpec, _ := spec["template"].(map[string]interface{})
	if podSpec != nil {
		if ps, ok := podSpec["spec"].(map[string]interface{}); ok {
			podSpec = ps
		}
	}

	return workloadDoc{
		kind:      kind,
		name:      name,
		namespace: namespace,
		labels:    labels,
		podSpec:   podSpec,
	}, true
}

// containers returns the pod spec containers, tolerating a missing pod spec.
func (w workloadDoc) containers() []map[string]interface{} {
	if w.podSpec == nil {
		return nil
	}
	list, _ := w.podSpec["containers"].([]interface{})
	out := make([]map[string]interface{}, 0, len(list))
	for _, c := range list {
		if cm, ok := c.(map[string]interface{}); ok {
			out = append(out, cm)
		}
	}
	return out
}

// SplitImage splits a container image reference into repository, tag and
// digest ("repo:tag@sha256:…" → repo, tag, digest).
func SplitImage(image string) (repo, tag, digest string) {
	ref := image
	if i := strings.LastIndex(ref, "@"); i >= 0 {
		digest = ref[i+1:]
		ref = ref[:i]
	}
	// A colon is a tag separator only when it comes after the last slash
	// (registry ports aside: host:port/path has the colon before the slash).
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i:], "/") {
		tag = ref[i+1:]
		repo = ref[:i]
		return repo, tag, digest
	}
	return ref, "", digest
}

// mainContainer selects the workload's main container: the container
// named after the component, else the first non-sidecar in spec.containers.
func mainContainer(containers []map[string]interface{}, componentName string) (map[string]interface{}, bool) {
	for _, c := range containers {
		if name, _ := c["name"].(string); name == componentName {
			return c, true
		}
	}
	for _, c := range containers {
		name, _ := c["name"].(string)
		if !knownSidecars[name] {
			return c, true
		}
	}
	return nil, false
}

// workloadVersion determines a workload group's version:
// main-container image tag (unless latest/missing) → app.kubernetes.io/version
// label → first 12 digest characters with a warning.
func workloadVersion(docs []workloadDoc, componentName string) (version, source string, warning string) {
	for _, w := range docs {
		containers := w.containers()
		main, ok := mainContainer(containers, componentName)
		if !ok {
			continue
		}
		image, _ := main["image"].(string)
		if image == "" {
			continue
		}
		_, tag, digest := SplitImage(image)
		if tag != "" && tag != "latest" {
			return tag, VersionImage, ""
		}
		// latest or no tag: fall back to the version label of this workload.
		if v := w.labels["app.kubernetes.io/version"]; v != "" {
			return v, VersionLabel, ""
		}
		if digest != "" {
			hex := strings.TrimPrefix(digest, "sha256:")
			if len(hex) > 12 {
				hex = hex[:12]
			}
			return hex, VersionImage, "no image tag, using image digest"
		}
	}
	// No usable image: try the version label of any workload in the group.
	for _, w := range docs {
		if v := w.labels["app.kubernetes.io/version"]; v != "" {
			return v, VersionLabel, ""
		}
	}
	return "", "", "no version could be determined"
}

// groupKey identifies one manifest component.
func groupKey(namespace, name string) string { return namespace + "\x00" + name }

// ManifestInput is one workload document ready for grouping: the parsed
// mapping plus the Flux Kustomization attribution.
type ManifestInput struct {
	Raw    map[string]interface{}
	FluxKs string
	Path   string
}

// CollectManifestComponents groups workload documents into components.
// Inputs with equal namespace/component-name merge into one
// component; the version comes from the highest-priority workload kind
// (Deployment > StatefulSet > DaemonSet > CronJob > Job). Workloads in one
// group reporting different versions yield the first and a warning.
func CollectManifestComponents(inputs []ManifestInput) []Component {
	type group struct {
		docs   []workloadDoc
		fluxKs string
		path   string
	}
	groups := map[string]*group{}
	order := []string{}

	for _, in := range inputs {
		w, ok := parseWorkloadDoc(in.Raw)
		if !ok {
			continue
		}
		key := groupKey(w.namespace, w.componentName())
		g, exists := groups[key]
		if !exists {
			g = &group{fluxKs: in.FluxKs, path: in.Path}
			groups[key] = g
			order = append(order, key)
		}
		g.docs = append(g.docs, w)
		if g.fluxKs == "" && in.FluxKs != "" {
			g.fluxKs = in.FluxKs
			g.path = in.Path
		}
	}

	components := make([]Component, 0, len(groups))
	for _, key := range order {
		g := groups[key]
		docs := make([]workloadDoc, len(g.docs))
		copy(docs, g.docs)
		sort.SliceStable(docs, func(i, j int) bool {
			ri, rj := workloadKindRank(docs[i].kind), workloadKindRank(docs[j].kind)
			if ri != rj {
				return ri < rj
			}
			return docs[i].name < docs[j].name
		})

		ns, name := docs[0].namespace, docs[0].componentName()
		c := Component{
			Source:    SourceManifest,
			Kind:      docs[0].kind,
			Namespace: ns,
			Name:      name,
			Software:  name,
			FluxKs:    g.fluxKs,
			Path:      g.path,
		}
		version, versionSource, warning := workloadVersion(docs, name)
		c.Version = version
		c.VersionSource = versionSource
		if warning != "" {
			c.Warnings = append(c.Warnings, warning)
		}
		c.Images = imagesOfDocs(docs)

		// Diverging versions across the group's workloads are worth a
		// warning even when a version was found.
		if c.Version != "" {
			for _, w := range docs[1:] {
				if v, _, _ := workloadVersion([]workloadDoc{w}, name); v != "" && v != c.Version {
					c.Warnings = append(c.Warnings, fmt.Sprintf("workloads in component report different versions (%s vs %s)", c.Version, v))
					break
				}
			}
		}

		components = append(components, c)
	}
	return components
}

// WorkloadGroupVersion determines a workload group's version from raw
// documents — the exported path for helm umbrella children, where the
// version is the main container's image tag (same fallbacks as manifest
// components). The source reports where the version came from; the warning
// explains fallbacks (digest, no version found) so callers surface them
// like manifest components do.
func WorkloadGroupVersion(raws []map[string]interface{}, componentName string) (version, source, warning string) {
	var docs []workloadDoc
	for _, raw := range raws {
		if w, ok := parseWorkloadDoc(raw); ok {
			docs = append(docs, w)
		}
	}
	if len(docs) == 0 {
		return "", "", ""
	}
	sort.SliceStable(docs, func(i, j int) bool {
		ri, rj := workloadKindRank(docs[i].kind), workloadKindRank(docs[j].kind)
		if ri != rj {
			return ri < rj
		}
		return docs[i].name < docs[j].name
	})
	return workloadVersion(docs, componentName)
}

// WorkloadGroupKind returns the highest-priority workload kind of the group
// ("" when the raws hold no workloads).
func WorkloadGroupKind(raws []map[string]interface{}) string {
	var docs []workloadDoc
	for _, raw := range raws {
		if w, ok := parseWorkloadDoc(raw); ok {
			docs = append(docs, w)
		}
	}
	if len(docs) == 0 {
		return ""
	}
	sort.SliceStable(docs, func(i, j int) bool {
		ri, rj := workloadKindRank(docs[i].kind), workloadKindRank(docs[j].kind)
		if ri != rj {
			return ri < rj
		}
		return docs[i].name < docs[j].name
	})
	return docs[0].kind
}

// WorkloadGroupNamespace returns the first non-empty namespace among the
// workloads.
func WorkloadGroupNamespace(raws []map[string]interface{}) string {
	for _, raw := range raws {
		if w, ok := parseWorkloadDoc(raw); ok && w.namespace != "" {
			return w.namespace
		}
	}
	return ""
}

// ImagesFromDocs collects the container images of workload documents:
// every regular and init container except known sidecars, deduplicated and
// sorted for deterministic output.
func ImagesFromDocs(raws []map[string]interface{}) []string {
	seen := map[string]bool{}
	docs := make([]workloadDoc, 0, len(raws))
	for _, raw := range raws {
		if w, ok := parseWorkloadDoc(raw); ok {
			docs = append(docs, w)
		}
	}
	collectImages(docs, seen)
	return sortedImages(seen)
}

// imagesOfDocs is ImagesFromDocs over already-parsed workloads.
func imagesOfDocs(docs []workloadDoc) []string {
	seen := map[string]bool{}
	collectImages(docs, seen)
	return sortedImages(seen)
}

func collectImages(docs []workloadDoc, seen map[string]bool) {
	for _, w := range docs {
		lists := [][]map[string]interface{}{w.containers()}
		if w.podSpec != nil {
			if inits, ok := w.podSpec["initContainers"].([]interface{}); ok {
				out := make([]map[string]interface{}, 0, len(inits))
				for _, c := range inits {
					if cm, ok := c.(map[string]interface{}); ok {
						out = append(out, cm)
					}
				}
				lists = append(lists, out)
			}
		}
		for _, containers := range lists {
			for _, c := range containers {
				name, _ := c["name"].(string)
				if knownSidecars[name] {
					continue
				}
				image, _ := c["image"].(string)
				if image != "" {
					seen[image] = true
				}
			}
		}
	}
}

func sortedImages(seen map[string]bool) []string {
	images := make([]string, 0, len(seen))
	for img := range seen {
		images = append(images, img)
	}
	sort.Strings(images)
	if len(images) == 0 {
		return nil
	}
	return images
}
