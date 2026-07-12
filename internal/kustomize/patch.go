package kustomize

import (
	"fmt"

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

	fsys := filesys.MakeFsInMemory()
	if err := fsys.WriteFile("/resources.yaml", resources); err != nil {
		return nil, fmt.Errorf("writing resources to in-memory fs: %w", err)
	}

	kust := types.Kustomization{
		Resources: []string{"resources.yaml"},
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
