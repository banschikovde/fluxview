package validate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// CRDYAMLToSchemaDir reads CRD definition files (.yaml/.yml, any depth) from
// dir and writes one kubeconform-named JSON Schema per CRD version into a
// schema directory, returning its path — or "" when no CRD documents were
// found. The returned directory can be used as a kubeconform schema
// location with the standard {{ .ResourceKind }}{{ .KindSuffix }}.json naming.
// An unreadable dir yields an error, so typos surface instead of silently
// producing no schemas.
//
// With cacheRoot non-empty the schemas live in a persistent directory
// <cacheRoot>/<hash of dir>/ and are reused across runs: a CRD file is
// reconverted only when its size or modification time changed, and schemas
// of files that disappeared are dropped. That directory must NOT be removed
// by the caller. With an empty cacheRoot a fresh temporary directory is
// returned instead — the caller removes it after validation finished
// (kubeconform reads schemas lazily).
func CRDYAMLToSchemaDir(dir, cacheRoot string) (string, error) {
	var out string
	persistent := cacheRoot != ""
	if persistent {
		hash := sha256.Sum256([]byte(mustAbs(dir)))
		out = filepath.Join(cacheRoot, hex.EncodeToString(hash[:8]))
		if err := os.MkdirAll(out, 0755); err != nil {
			return "", fmt.Errorf("creating CRD schema cache dir: %w", err)
		}
	} else {
		temp, err := os.MkdirTemp("", "fluxview-crd-schemas-")
		if err != nil {
			return "", fmt.Errorf("creating temp schema dir: %w", err)
		}
		out = temp
	}

	meta := loadCRDCacheMeta(out)
	seen := make(map[string]struct{})

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		name := info.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			return nil
		}

		key := mustAbs(path)
		seen[key] = struct{}{}
		if entry, fresh := meta.Files[key]; fresh && entry.ModTime == info.ModTime().UnixNano() && entry.Size == info.Size() && outputsExist(out, entry.Outputs) {
			return nil
		}

		outputs, cerr := crdFileToSchemas(path, out)
		if cerr != nil {
			return cerr
		}
		// Physical removal of stale files is left to the sweep below: two
		// source files may claim the same schema name (same kind defined
		// twice), and only the full claim set knows which files are truly
		// orphaned.
		meta.Files[key] = crdCacheEntry{ModTime: info.ModTime().UnixNano(), Size: info.Size(), Outputs: outputs}
		return nil
	})
	if err != nil {
		if !persistent {
			_ = os.RemoveAll(out) // don't leave a temp dir behind on failure
		}
		return "", fmt.Errorf("converting CRDs from %s: %w", dir, err)
	}

	// Forget CRD files that disappeared since the last run.
	for key := range meta.Files {
		if _, stillThere := seen[key]; !stillThere {
			delete(meta.Files, key)
		}
	}

	// Sweep the directory to the claimed set: removes schemas of removed
	// or changed sources, and orphans left by an interrupted run (outputs
	// written but meta.json not yet saved, temp files).
	live := map[string]struct{}{"meta.json": {}}
	for _, entry := range meta.Files {
		for _, name := range entry.Outputs {
			live[name] = struct{}{}
		}
	}
	if entries, rerr := os.ReadDir(out); rerr == nil {
		for _, e := range entries {
			if _, kept := live[e.Name()]; !kept {
				_ = os.Remove(filepath.Join(out, e.Name()))
			}
		}
	}

	if persistent {
		if err := saveCRDCacheMeta(out, meta); err != nil {
			return "", err
		}
	}

	return dropEmptyDir(out, persistent), nil
}

// crdCacheMeta is the on-disk state of the CRD conversion cache: which
// source files were converted, when, into which schema files.
type crdCacheMeta struct {
	Files map[string]crdCacheEntry `json:"files"`
}

// crdCacheEntry pins a converted source file by size and mtime.
type crdCacheEntry struct {
	ModTime int64    `json:"mtime"` // UnixNano
	Size    int64    `json:"size"`
	Outputs []string `json:"outputs"`
}

func loadCRDCacheMeta(dir string) crdCacheMeta {
	meta := crdCacheMeta{Files: map[string]crdCacheEntry{}}
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return meta // missing or unreadable: convert everything fresh
	}
	_ = json.Unmarshal(data, &meta)
	if meta.Files == nil {
		meta.Files = map[string]crdCacheEntry{}
	}
	return meta
}

func saveCRDCacheMeta(dir string, meta crdCacheMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encoding CRD cache meta: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, "meta.json"), data); err != nil {
		return fmt.Errorf("writing CRD cache meta: %w", err)
	}
	return nil
}

func outputsExist(dir string, outputs []string) bool {
	for _, name := range outputs {
		if !fileExists(filepath.Join(dir, name)) {
			return false
		}
	}
	return true
}

func mustAbs(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// dropEmptyDir returns "" when no schemas were written, so callers skip
// adding an empty schema location. In temp mode the directory is removed;
// the persistent cache dir stays for the next run (meta.json alone does
// not make it useful).
func dropEmptyDir(dir string, persistent bool) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return dir
	}
	for _, e := range entries {
		if persistent && e.Name() == "meta.json" {
			continue
		}
		return dir // a real schema file
	}
	if !persistent {
		_ = os.RemoveAll(dir)
	}
	return ""
}

// crdFileToSchemas converts every CRD document in one YAML file, returning
// the schema files written.
func crdFileToSchemas(path, outDir string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	defer f.Close()

	var outputs []string
	decoder := yaml.NewYAMLToJSONDecoder(f)
	for {
		var crd apiextv1.CustomResourceDefinition
		if err := decoder.Decode(&crd); err != nil {
			if err == io.EOF {
				return outputs, nil
			}
			// A non-EOF decode error means a malformed/truncated document.
			// This is a schema source the user pointed validate at: the
			// kinds it defines would silently lose validation — fail the
			// run like any other unusable schema source, and make sure
			// nothing of this file reaches the cache (the error propagates
			// before meta.json is saved, so every run reports it).
			return nil, fmt.Errorf("could not decode a document in %s: %w", path, err)
		}
		if crd.Kind != "CustomResourceDefinition" || crd.Spec.Group == "" {
			continue
		}

		for _, ver := range crd.Spec.Versions {
			if ver.Schema == nil || ver.Schema.OpenAPIV3Schema == nil {
				continue
			}
			// The conversion calls below are effectively infallible on a
			// decodable document (field mapping with no failure sources),
			// so these errors are defense-in-depth: if a future k8s.io
			// upgrade ever makes one real, the gate fails closed instead
			// of silently losing the kind. Escape hatch: drop the CRD file
			// from --schema-dir.
			internal := &apiextensions.JSONSchemaProps{}
			if err := apiextv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(ver.Schema.OpenAPIV3Schema, internal, nil); err != nil {
				return nil, fmt.Errorf("could not convert CRD schema for %s/%s %s: %w", crd.Spec.Group, ver.Name, crd.Spec.Names.Kind, err)
			}
			schema := &spec.Schema{}
			if err := validation.ConvertJSONSchemaProps(internal, schema); err != nil {
				return nil, fmt.Errorf("could not convert CRD schema for %s/%s %s: %w", crd.Spec.Group, ver.Name, crd.Spec.Names.Kind, err)
			}
			data, err := json.Marshal(schema)
			if err != nil {
				return nil, fmt.Errorf("could not marshal CRD schema for %s/%s %s: %w", crd.Spec.Group, ver.Name, crd.Spec.Names.Kind, err)
			}

			name := kubeconformSchemaName(crd.Spec.Names.Kind, crd.Spec.Group, ver.Name)
			if err := os.WriteFile(filepath.Join(outDir, name), data, 0644); err != nil {
				return outputs, fmt.Errorf("writing schema %s: %w", name, err)
			}
			outputs = append(outputs, name)
		}
	}
}

// kubeconformSchemaName builds the filename kubeconform's local registry
// requests for a kind: <lowercase kind>-<first group label>-<version>.json
// ("helm.toolkit.fluxcd.io/v2, HelmRelease" → "helmrelease-helm-v2.json").
func kubeconformSchemaName(kind, group, version string) string {
	firstLabel := group
	if i := strings.Index(group, "."); i >= 0 {
		firstLabel = group[:i]
	}
	return fmt.Sprintf("%s-%s-%s.json", strings.ToLower(kind), strings.ToLower(firstLabel), version)
}
