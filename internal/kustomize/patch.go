package kustomize

import (
	"fmt"
	"strings"

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
// If a patch target doesn't match any resource, the patch is silently
// skipped (matching Flux/kustomize behavior).
func ApplyPatches(resources []byte, patches []PatchSpec) ([]byte, error) {
	if len(patches) == 0 {
		return resources, nil
	}

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

	kust := types.Kustomization{
		Resources: resourceFiles,
	}
	for _, p := range patches {
		kp := types.Patch{
			Patch: p.Patch,
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
		return nil, fmt.Errorf("applying patches: %w", err)
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
func splitYAMLDocs(data []byte) []string {
	var docs []string
	for _, doc := range strings.Split(string(data), "\n---") {
		d := strings.TrimSpace(doc)
		if d != "" {
			docs = append(docs, d)
		}
	}
	return docs
}
