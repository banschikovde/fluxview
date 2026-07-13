package kustomize

import (
	"fmt"
	"os"
	"strings"

	"github.com/cyphar/filepath-securejoin"
	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
	"sigs.k8s.io/kustomize/kyaml/resid"
)

// PatchSpec mirrors kustomize's target+patch shape — shared by
// Kustomization.spec.patches and HelmRelease.spec.postRenderers.
type PatchSpec struct {
	Target *PatchTarget `yaml:"target,omitempty"`
	Patch  string       `yaml:"patch,omitempty"`
	Path   string       `yaml:"path,omitempty"` // patch from a separate file
}

// PatchTarget specifies which resources a patch applies to.
type PatchTarget struct {
	Group              string `yaml:"group,omitempty"`
	Version            string `yaml:"version,omitempty"`
	Kind               string `yaml:"kind,omitempty"`
	Name               string `yaml:"name,omitempty"`
	Namespace          string `yaml:"namespace,omitempty"`
	LabelSelector      string `yaml:"labelSelector,omitempty"`
	AnnotationSelector string `yaml:"annotationSelector,omitempty"`
}

// ApplyPatches applies kustomize-style patches (JSON6902) to an already
// materialized set of resources, in memory. No directory on disk needed.
// baseDir restricts patches[].path resolution — any path escaping baseDir
// is rejected (prevents path traversal from untrusted repo content).
// If a patch target doesn't match any resource, the patch is silently
// skipped (matching Flux/kustomize behavior).
func ApplyPatches(resources []byte, patches []PatchSpec, baseDir string) ([]byte, error) {
	if len(patches) == 0 {
		return resources, nil
	}

	kust := types.Kustomization{}
	for _, p := range patches {
		// If Path is set, read patch content from file (with path traversal protection).
		patchContent := p.Patch
		if p.Path != "" {
			resolved, err := securejoin.SecureJoin(baseDir, p.Path)
			if err != nil {
				return nil, fmt.Errorf("resolving patch path %s: %w", p.Path, err)
			}
			if !IsPathWithinRoot(resolved, baseDir) {
				return nil, fmt.Errorf("patch path %s escapes base directory", p.Path)
			}
			data, err := os.ReadFile(resolved)
			if err != nil {
				return nil, fmt.Errorf("reading patch file %s: %w", p.Path, err)
			}
			patchContent = string(data)
		}
		kp := types.Patch{
			Patch: patchContent,
		}
		if p.Target != nil {
			kp.Target = &types.Selector{
				ResId: resid.ResId{
					Gvk: resid.Gvk{
						Group:   p.Target.Group,
						Version: p.Target.Version,
						Kind:    p.Target.Kind,
					},
					Name:      p.Target.Name,
					Namespace: p.Target.Namespace,
				},
			}
			if p.Target.LabelSelector != "" {
				kp.Target.LabelSelector = p.Target.LabelSelector
			}
			if p.Target.AnnotationSelector != "" {
				kp.Target.AnnotationSelector = p.Target.AnnotationSelector
			}
		}
		kust.Patches = append(kust.Patches, kp)
	}

	return runInMemoryBuild(resources, kust)
}

// ApplyTargetNamespace sets metadata.namespace on all namespaced resources to
// the given namespace, mimicking Flux's Kustomization.spec.targetNamespace.
// It reuses kustomize's native namespace transformer, which overrides existing
// namespaces and automatically skips cluster-scoped resources. If namespace is
// empty the input is returned unchanged.
func ApplyTargetNamespace(resources []byte, namespace string) ([]byte, error) {
	if namespace == "" {
		return resources, nil
	}
	kust := types.Kustomization{
		Namespace: namespace,
	}
	return runInMemoryBuild(resources, kust)
}

// runInMemoryBuild runs an in-memory kustomize build over the given resources.
// Resources are deduplicated and written to an in-memory filesystem, then built
// with the additional kustomization fields set in kust (Resources is filled
// automatically). Shared by ApplyPatches and ApplyTargetNamespace.
func runInMemoryBuild(resources []byte, kust types.Kustomization) ([]byte, error) {
	// Deduplicate input resources — kustomize rejects duplicate resource IDs.
	// Last occurrence wins (matches kustomize ResMap behavior).
	resources = dedupInputDocs(resources)

	fsys := filesys.MakeFsInMemory()

	// Split multi-doc YAML into separate files — kustomize works best with
	// individual resource files rather than a single multi-doc file.
	var resourceFiles []string
	for i, doc := range splitYAMLDocs(resources) {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}
		name := fmt.Sprintf("/resource-%d.yaml", i)
		if err := fsys.WriteFile(name, []byte(doc)); err != nil {
			return nil, fmt.Errorf("writing resource to in-memory fs: %w", err)
		}
		resourceFiles = append(resourceFiles, name)
	}
	kust.Resources = resourceFiles

	kustYAML, err := yaml.Marshal(kust)
	if err != nil {
		return nil, fmt.Errorf("marshaling kustomization: %w", err)
	}
	if err := fsys.WriteFile("/kustomization.yaml", kustYAML); err != nil {
		return nil, fmt.Errorf("writing kustomization: %w", err)
	}

	opts := krusty.MakeDefaultOptions()
	kustomizer := krusty.MakeKustomizer(opts)
	resMap, err := kustomizer.Run(fsys, "/")
	if err != nil {
		return nil, fmt.Errorf("running in-memory kustomize build: %w", err)
	}

	return resMap.AsYaml()
}

// dedupInputDocs removes duplicate YAML documents by apiVersion/kind/namespace/name.
// Last occurrence wins. Required because kustomize's resource accumulator rejects
// duplicate IDs even across separate files.
func dedupInputDocs(data []byte) []byte {
	type key struct {
		group, kind, ns, name string
	}
	seen := make(map[key]int)
	var result []string

	for _, doc := range splitYAMLDocs(data) {
		var meta struct {
			APIVersion string `yaml:"apiVersion"`
			Kind       string `yaml:"kind"`
			Metadata   struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil || meta.Kind == "" || meta.Metadata.Name == "" {
			result = append(result, doc)
			continue
		}
		group := ""
		if idx := strings.Index(meta.APIVersion, "/"); idx > 0 {
			group = meta.APIVersion[:idx]
		}
		k := key{group, meta.Kind, meta.Metadata.Namespace, meta.Metadata.Name}
		if i, ok := seen[k]; ok {
			result[i] = doc // replace
		} else {
			seen[k] = len(result)
			result = append(result, doc)
		}
	}

	return []byte(strings.Join(result, "\n---\n"))
}

// splitYAMLDocs splits multi-doc YAML on document separators.
// A separator is a line that is exactly "---" at column 0 (no indentation),
// matching the same logic as flux.SplitYAMLText to avoid false splits on
// content like -----BEGIN CERTIFICATE----- inside block scalars.
func splitYAMLDocs(data []byte) []string {
	lines := strings.Split(string(data), "\n")
	var docs []string
	var current []string

	flush := func() {
		if len(current) == 0 {
			return
		}
		doc := strings.TrimSpace(strings.Join(current, "\n"))
		if doc != "" {
			docs = append(docs, doc)
		}
		current = nil
	}

	for _, line := range lines {
		if strings.TrimRight(line, " \t") == "---" {
			flush()
			continue
		}
		current = append(current, line)
	}
	flush()

	return docs
}
