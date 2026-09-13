package kustomize

import (
	"fmt"
	"os"
	"strings"

	"github.com/banschikovde/fluxview/internal/yamlutil"
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

// ImageOverride mirrors kustomize's image-transformer entry (sigs.k8s.io/kustomize/api/types.Image),
// shared by Kustomization.spec.images. Fields match the Flux spec: an image is
// identified by Name and overridden via NewName/NewTag/TagSuffix/Digest.
type ImageOverride struct {
	Name      string `yaml:"name,omitempty"`
	NewName   string `yaml:"newName,omitempty"`
	NewTag    string `yaml:"newTag,omitempty"`
	TagSuffix string `yaml:"tagSuffix,omitempty"`
	Digest    string `yaml:"digest,omitempty"`
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

	kp, err := collectPatches(patches, baseDir)
	if err != nil {
		return nil, err
	}

	kust := types.Kustomization{Patches: kp}
	return runInMemoryBuild(resources, kust)
}

// ApplyTransformations applies kustomize-style patches (JSON6902), image
// overrides, and the target namespace to an already materialized set of
// resources in ONE in-memory kustomize build. All three live as fields of a
// single types.Kustomization, so the old ApplyPatches → ApplyImages →
// ApplyTargetNamespace chain cost three full parse → build → serialize
// cycles for what one build produces.
//
// Transformer order inside the build (kustomize api v0.21.1,
// kusttarget_configplugin.go configureBuiltinTransformers):
// PatchTransformer (#2) → NamespaceTransformer (#3) → ImageTagTransformer
// (#10) — patches, then namespace, then images. The old sequential chain
// applied images BEFORE the namespace; the swap is behaviorally
// unobservable — the two transformers touch disjoint fields and neither's
// matching depends on the other's output. The observable contracts (patches
// run first: patch selectors match pre-namespace namespaces, and
// patch-rewritten images are subject to image overrides) are locked by
// TestApplyTransformations_Ordering_*. baseDir restricts patches[].path
// resolution (path traversal protection). Empty patches/images/namespace
// parts are simply skipped; with nothing to apply the input is returned
// unchanged.
func ApplyTransformations(resources []byte, patches []PatchSpec, images []ImageOverride, namespace, baseDir string) ([]byte, error) {
	if len(patches) == 0 && len(images) == 0 && namespace == "" {
		return resources, nil
	}

	kust := types.Kustomization{Namespace: namespace}

	kp, err := collectPatches(patches, baseDir)
	if err != nil {
		return nil, err
	}
	kust.Patches = kp

	for _, img := range images {
		kust.Images = append(kust.Images, types.Image{
			Name:      img.Name,
			NewName:   img.NewName,
			NewTag:    img.NewTag,
			TagSuffix: img.TagSuffix,
			Digest:    img.Digest,
		})
	}

	return runInMemoryBuild(resources, kust)
}

// collectPatches resolves PatchSpec entries into kustomize patches. Patch
// content comes from the inline Patch field or is read from Path (resolved
// under baseDir — any path escaping it is rejected, preventing path
// traversal from untrusted repo content).
func collectPatches(patches []PatchSpec, baseDir string) ([]types.Patch, error) {
	var collected []types.Patch
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
		collected = append(collected, kp)
	}
	return collected, nil
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

// inMemoryKustomizer is the shared kustomizer for all in-memory builds.
// Constructing a kustomizer builds the whole transformer pipeline, so it is
// done once at package init. Run is safe for the CLI's sequential builds;
// if builds ever become concurrent, guard Run calls with a mutex —
// krusty.Kustomizer does not guarantee parallel-Run safety.
var inMemoryKustomizer = krusty.MakeKustomizer(krusty.MakeDefaultOptions())

// runInMemoryBuild runs an in-memory kustomize build over the given resources.
// Resources are deduplicated and written to an in-memory filesystem, then built
// with the additional kustomization fields set in kust (Resources is filled
// automatically). Shared by ApplyPatches, ApplyTransformations and
// ApplyTargetNamespace.
func runInMemoryBuild(resources []byte, kust types.Kustomization) ([]byte, error) {
	// Deduplicate input resources — kustomize rejects duplicate resource IDs.
	// Last occurrence wins (matches kustomize ResMap behavior).
	resources = yamlutil.DedupDocs(resources)

	fsys := filesys.MakeFsInMemory()

	// Split multi-doc YAML into separate files — kustomize works best with
	// individual resource files rather than a single multi-doc file.
	var resourceFiles []string
	for i, doc := range yamlutil.SplitYAMLText(resources) {
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

	resMap, err := inMemoryKustomizer.Run(fsys, "/")
	if err != nil {
		return nil, fmt.Errorf("running in-memory kustomize build: %w", err)
	}

	return resMap.AsYaml()
}
