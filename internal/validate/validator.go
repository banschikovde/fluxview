// Package validate provides resource validation against schema files.
// Supports two formats:
//   - JSON Schema files (.json) — kubeconform-compatible, e.g. Flux crd-schemas.tar.gz
//   - CRD definition files (.yaml/.yml) — standard Kubernetes CustomResourceDefinition manifests
package validate

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// fluxSchemaKinds maps Flux crd-schemas.tar.gz filenames (without extension)
// to their GroupKind. This is an explicit mapping because the lowercase
// filename → CamelCase kind conversion is ambiguous (e.g. "helmrelease" →
// "HelmRelease", not "Helmrelease").
var fluxSchemaKinds = map[string]schema.GroupKind{
	"kustomization-kustomize-v1":       {Group: "kustomize.toolkit.fluxcd.io", Kind: "Kustomization"},
	"helmrelease-helm-v2":              {Group: "helm.toolkit.fluxcd.io", Kind: "HelmRelease"},
	"gitrepository-source-v1":          {Group: "source.toolkit.fluxcd.io", Kind: "GitRepository"},
	"helmrepository-source-v1":         {Group: "source.toolkit.fluxcd.io", Kind: "HelmRepository"},
	"helmchart-source-v1":              {Group: "source.toolkit.fluxcd.io", Kind: "HelmChart"},
	"bucket-source-v1":                 {Group: "source.toolkit.fluxcd.io", Kind: "Bucket"},
	"ocirepository-source-v1":          {Group: "source.toolkit.fluxcd.io", Kind: "OCIRepository"},
	"alert-notification-v1beta3":       {Group: "notification.toolkit.fluxcd.io", Kind: "Alert"},
	"provider-notification-v1beta3":    {Group: "notification.toolkit.fluxcd.io", Kind: "Provider"},
	"receiver-notification-v1":         {Group: "notification.toolkit.fluxcd.io", Kind: "Receiver"},
	"imagerepository-image-v1":         {Group: "image.toolkit.fluxcd.io", Kind: "ImageRepository"},
	"imagepolicy-image-v1":             {Group: "image.toolkit.fluxcd.io", Kind: "ImagePolicy"},
	"imageupdateautomation-image-v1":   {Group: "image.toolkit.fluxcd.io", Kind: "ImageUpdateAutomation"},
	"artifactgenerator-source-v1beta1": {Group: "source.toolkit.fluxcd.io", Kind: "ArtifactGenerator"},
	"externalartifact-source-v1":       {Group: "source.toolkit.fluxcd.io", Kind: "ExternalArtifact"},
}

// Validator validates Kubernetes resources against bundled schema files.
// Resources without a matching schema are silently skipped.
type Validator struct {
	schemas map[schema.GroupKind]validation.SchemaValidator
}

// New creates a Validator by loading schema files from schemaDir.
// The directory may contain .json (kubeconform format) and .yaml (CRD
// definition) files, including in subdirectories. If schemaDir is empty
// or doesn't exist, returns an empty Validator (all resources skipped).
func New(schemaDir string) *Validator {
	v := &Validator{
		schemas: make(map[schema.GroupKind]validation.SchemaValidator),
	}

	if schemaDir == "" {
		return v
	}
	if info, err := os.Stat(schemaDir); err != nil || !info.IsDir() {
		return v
	}

	// Load shared JSON Schema definitions first (e.g. _definitions.json).
	defs := loadDefinitions(schemaDir)

	// Recursively load all schema files.
	filepath.Walk(schemaDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		name := info.Name()

		switch {
		case strings.HasSuffix(name, ".json") && name != "_definitions.json" && name != "all.json":
			v.loadJSONSchema(path, defs)
		case strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml"):
			v.loadCRDFile(path)
		}
		return nil
	})

	return v
}

// loadDefinitions loads _definitions.json from the schema directory root.
func loadDefinitions(dir string) map[string]interface{} {
	data, err := os.ReadFile(filepath.Join(dir, "_definitions.json"))
	if err != nil {
		return nil
	}
	var raw map[string]interface{}
	if json.Unmarshal(data, &raw) != nil {
		log.Printf("Warning: failed to parse _definitions.json: %v", err)
		return nil
	}
	return raw
}

// loadJSONSchema loads a kubeconform-compatible JSON Schema file.
func (v *Validator) loadJSONSchema(path string, defs map[string]interface{}) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("Warning: could not read %s: %v", path, err)
		return
	}

	var raw map[string]interface{}
	if json.Unmarshal(data, &raw) != nil {
		log.Printf("Warning: could not parse JSON schema %s", path)
		return
	}

	// Merge shared definitions into this schema.
	if defs != nil {
		if defMap, ok := defs["definitions"].(map[string]interface{}); ok {
			existing, _ := raw["definitions"].(map[string]interface{})
			if existing == nil {
				existing = map[string]interface{}{}
			}
			for k, val := range defMap {
				existing[k] = val
			}
			raw["definitions"] = existing
		}
	}

	// Fix $ref paths: _definitions.json#/definitions/X → #/definitions/X.
	fixRefs(raw)

	fixed, err := json.Marshal(raw)
	if err != nil {
		return
	}

	var s spec.Schema
	if json.Unmarshal(fixed, &s) != nil {
		log.Printf("Warning: could not parse OpenAPI schema %s", path)
		return
	}

	// Look up group/kind from the explicit Flux schema filename map.
	gk, ok := fluxSchemaKinds[strings.TrimSuffix(filepath.Base(path), ".json")]
	if !ok {
		log.Printf("Warning: skipping %s — unknown schema filename (use YAML CRD format for non-Flux CRDs)", path)
		return
	}

	validator := validation.NewSchemaValidatorFromOpenAPI(&s)
	v.schemas[gk] = validator
}

// loadCRDFile loads CRD definitions from a YAML file (one or more documents).
func (v *Validator) loadCRDFile(path string) {
	f, err := os.Open(path)
	if err != nil {
		log.Printf("Warning: could not read %s: %v", path, err)
		return
	}
	defer f.Close()

	decoder := yaml.NewYAMLToJSONDecoder(f)
	for {
		var crd apiextv1.CustomResourceDefinition
		if err := decoder.Decode(&crd); err != nil {
			break
		}
		if crd.Kind != "CustomResourceDefinition" {
			continue
		}

		gk := schema.GroupKind{Group: crd.Spec.Group, Kind: crd.Spec.Names.Kind}
		for _, ver := range crd.Spec.Versions {
			if ver.Schema == nil || ver.Schema.OpenAPIV3Schema == nil {
				continue
			}
			internal := &apiextensions.JSONSchemaProps{}
			if err := apiextv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(ver.Schema.OpenAPIV3Schema, internal, nil); err != nil {
				log.Printf("Warning: could not convert CRD schema for %s: %v", gk, err)
				continue
			}
			validator, _, err := validation.NewSchemaValidator(internal)
			if err != nil {
				log.Printf("Warning: could not create validator for %s: %v", gk, err)
				continue
			}
			v.schemas[gk] = validator
		}
	}
}

// fixRefs recursively replaces "_definitions.json#/definitions/" with "#/definitions/"
// in all $ref values within the raw schema map.
func fixRefs(m map[string]interface{}) {
	for k, val := range m {
		switch v := val.(type) {
		case string:
			if k == "$ref" {
				m[k] = strings.Replace(v, "_definitions.json#/definitions/", "#/definitions/", 1)
			}
		case map[string]interface{}:
			fixRefs(v)
		case []interface{}:
			for _, item := range v {
				if nested, ok := item.(map[string]interface{}); ok {
					fixRefs(nested)
				}
			}
		}
	}
}

// SchemaCount returns the number of loaded schemas.
func (v *Validator) SchemaCount() int {
	return len(v.schemas)
}

// Result holds validation errors for a single resource.
type Result struct {
	Resource string
	Errors   []string
}

// resourceMeta holds the identifying fields of a Kubernetes resource.
type resourceMeta struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
}

// Validate checks resources in multi-doc YAML against loaded schemas.
// Resources without a matching schema are silently skipped.
func (v *Validator) Validate(data []byte) []Result {
	var results []Result

	for _, doc := range splitYAMLDocs(data) {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" {
			continue
		}

		// Single unmarshal — extract metadata from the same object.
		var obj map[string]interface{}
		if yaml.Unmarshal([]byte(trimmed), &obj) != nil {
			continue
		}

		meta, ok := extractMeta(obj)
		if !ok {
			continue
		}

		group, _ := parseAPIVersion(meta.APIVersion)
		gk := schema.GroupKind{Group: group, Kind: meta.Kind}
		validator, ok := v.schemas[gk]
		if !ok {
			continue // No schema — skip silently.
		}

		errs := validation.ValidateCustomResource(nil, obj, validator)
		if len(errs) == 0 {
			continue
		}

		name := meta.Name
		if meta.Namespace != "" {
			name = meta.Namespace + "/" + meta.Name
		}

		var messages []string
		for _, e := range errs {
			messages = append(messages, e.Error())
		}
		results = append(results, Result{
			Resource: fmt.Sprintf("%s %s", meta.Kind, name),
			Errors:   messages,
		})
	}

	return results
}

// extractMeta pulls group/kind/name/namespace from a parsed resource map.
func extractMeta(obj map[string]interface{}) (resourceMeta, bool) {
	meta := resourceMeta{}

	apiVersion, ok := obj["apiVersion"].(string)
	if !ok || apiVersion == "" {
		return meta, false
	}
	kind, ok := obj["kind"].(string)
	if !ok || kind == "" {
		return meta, false
	}
	meta.APIVersion = apiVersion
	meta.Kind = kind

	if md, ok := obj["metadata"].(map[string]interface{}); ok {
		meta.Name, _ = md["name"].(string)
		meta.Namespace, _ = md["namespace"].(string)
	}

	return meta, true
}

// parseAPIVersion splits apiVersion into group and version.
func parseAPIVersion(apiVersion string) (group, version string) {
	parts := strings.SplitN(apiVersion, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "", parts[0]
}

// splitYAMLDocs splits multi-doc YAML by --- separators (text-based, fast).
func splitYAMLDocs(data []byte) []string {
	var docs []string
	for _, doc := range strings.Split(string(data), "\n---") {
		s := strings.TrimSpace(doc)
		s = strings.TrimPrefix(s, "---")
		s = strings.TrimSpace(s)
		if s != "" {
			docs = append(docs, s)
		}
	}
	return docs
}
