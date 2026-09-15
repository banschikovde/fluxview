package validate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// convertWithCache runs CRDYAMLToSchemaDir with a persistent cache root.
func convertWithCache(t *testing.T, dir, cacheRoot string) string {
	t.Helper()
	out, err := CRDYAMLToSchemaDir(dir, cacheRoot)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCRDYAMLToSchemaDir_Cache(t *testing.T) {
	dir := t.TempDir()
	cache := t.TempDir()
	writeFile(t, dir, "crd.yaml", testCRDYAML)

	out := convertWithCache(t, dir, cache)
	widgetPath := filepath.Join(out, "widget-test-v1.json")
	if !fileExists(widgetPath) {
		t.Fatalf("expected converted schema %s", widgetPath)
	}

	// Corrupt the cached schema and reconvert without touching the source:
	// the cache entry is still fresh (same mtime+size), so the corruption
	// must survive — proving no reconversion happened.
	if err := os.WriteFile(widgetPath, []byte("CORRUPTED"), 0644); err != nil {
		t.Fatal(err)
	}
	again := convertWithCache(t, dir, cache)
	if again != out {
		t.Fatalf("cache dir changed between runs: %q → %q", out, again)
	}
	if data, _ := os.ReadFile(widgetPath); string(data) != "CORRUPTED" {
		t.Error("an unchanged source must not be reconverted; the cached schema was rewritten")
	}

	// Touch the source (new mtime, same content): reconversion must run
	// and repair the schema.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "crd.yaml"), future, future); err != nil {
		t.Fatal(err)
	}
	convertWithCache(t, dir, cache)
	data, err := os.ReadFile(widgetPath)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Errorf("a touched source must be reconverted to valid JSON: %v", err)
	}
}

func TestCRDYAMLToSchemaDir_CacheDropsRemovedFiles(t *testing.T) {
	dir := t.TempDir()
	cache := t.TempDir()
	writeFile(t, dir, "widget.yaml", testCRDYAML)
	gadgetYAML := `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: gadgets.test.example.com
spec:
  group: test.example.com
  names:
    kind: Gadget
    plural: gadgets
  scope: Namespaced
  versions:
    - name: v1
      served: true
      storage: true
      schema:
        openAPIV3Schema:
          type: object
`
	writeFile(t, dir, "gadget.yaml", gadgetYAML)

	out := convertWithCache(t, dir, cache)
	for _, name := range []string{"widget-test-v1.json", "gadget-test-v1.json"} {
		if !fileExists(filepath.Join(out, name)) {
			t.Fatalf("expected converted schema %s", name)
		}
	}

	// Remove one CRD file: its schema disappears on the next run, the
	// other one survives.
	if err := os.Remove(filepath.Join(dir, "gadget.yaml")); err != nil {
		t.Fatal(err)
	}
	convertWithCache(t, dir, cache)
	if fileExists(filepath.Join(out, "gadget-test-v1.json")) {
		t.Error("schema of a removed CRD file must be dropped from the cache")
	}
	if !fileExists(filepath.Join(out, "widget-test-v1.json")) {
		t.Error("the untouched CRD schema must survive")
	}
}

func TestCRDYAMLToSchemaDir_CacheNoCRDs(t *testing.T) {
	dir := t.TempDir()
	cache := t.TempDir()

	if out := convertWithCache(t, dir, cache); out != "" {
		t.Errorf("expected empty result for a dir without CRDs, got %q", out)
	}
}

// brokenCRDYAML is syntactically invalid YAML (tab indentation).
const brokenCRDYAML = "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nspec:\n\tgroup: broken\n"

// TestCRDYAMLToSchemaDir_BrokenYAMLFails pins the validation-gate contract:
// a malformed CRD file in the schema dir is a hard error — the kinds it
// defines would otherwise silently lose validation — never a warn-and-skip.
func TestCRDYAMLToSchemaDir_BrokenYAMLFails(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "good.yaml", testCRDYAML)
	writeFile(t, dir, "broken.yaml", brokenCRDYAML)

	if out, err := CRDYAMLToSchemaDir(dir, ""); err == nil {
		t.Fatalf("a broken CRD file must fail the conversion, got dir %q", out)
	} else if !strings.Contains(err.Error(), "could not decode") {
		t.Errorf("error should name the decode failure, got: %v", err)
	}
}

// TestCRDYAMLToSchemaDir_BrokenYAMLNotCached pins the warn-once fix: a
// decode failure is never cached — every run reports it again until the
// file is fixed, and a broken file cannot poison the cache state.
func TestCRDYAMLToSchemaDir_BrokenYAMLNotCached(t *testing.T) {
	dir := t.TempDir()
	cache := t.TempDir()
	crdPath := filepath.Join(dir, "crd.yaml")
	writeFile(t, dir, "crd.yaml", testCRDYAML)

	if out := convertWithCache(t, dir, cache); out == "" {
		t.Fatal("expected the good CRD to convert")
	}

	// Break the file: every following run must fail the same way.
	if err := os.WriteFile(crdPath, []byte(brokenCRDYAML), 0644); err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 2; run++ {
		if _, err := CRDYAMLToSchemaDir(dir, cache); err == nil {
			t.Fatalf("run %d: a broken CRD file must fail on a warm cache too", run)
		}
	}

	// Repair: the run succeeds again and produces a usable schema.
	future := time.Now().Add(2 * time.Hour)
	if err := os.WriteFile(crdPath, []byte(testCRDYAML), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(crdPath, future, future); err != nil {
		t.Fatal(err)
	}
	out := convertWithCache(t, dir, cache)
	if !fileExists(filepath.Join(out, "widget-test-v1.json")) {
		t.Error("a repaired CRD file must convert again")
	}
}

// TestCRDYAMLToSchemaDir_NonCRDYAMLStillSkipped keeps the neighbor contract:
// a well-formed YAML file that is simply not a CRD does not fail the run.
func TestCRDYAMLToSchemaDir_NonCRDYAMLStillSkipped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "crd.yaml", testCRDYAML)
	writeFile(t, dir, "notes.yaml", "just: some\nnotes: true\n")

	out := convertWithCache(t, dir, t.TempDir())
	if !fileExists(filepath.Join(out, "widget-test-v1.json")) {
		t.Error("the real CRD should still convert")
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 2 { // the schema + meta.json
		t.Errorf("expected only the schema and meta.json, got %d entries", len(entries))
	}
}

// TestCRDYAMLToSchemaDir_CacheSharedOutputName pins the claim-set rule:
// when two source files produce the same schema file (the same kind
// defined twice), removing one file must not delete the schema the other
// still claims.
func TestCRDYAMLToSchemaDir_CacheSharedOutputName(t *testing.T) {
	dir := t.TempDir()
	cache := t.TempDir()
	writeFile(t, dir, "a.yaml", testCRDYAML)
	writeFile(t, dir, "b.yaml", testCRDYAML) // same kind → same output name

	out := convertWithCache(t, dir, cache)
	widgetPath := filepath.Join(out, "widget-test-v1.json")
	if !fileExists(widgetPath) {
		t.Fatal("expected the shared converted schema")
	}

	if err := os.Remove(filepath.Join(dir, "b.yaml")); err != nil {
		t.Fatal(err)
	}
	convertWithCache(t, dir, cache)
	if !fileExists(widgetPath) {
		t.Error("a schema claimed by a surviving source must not be swept")
	}
}

// TestCRDYAMLToSchemaDir_CacheIsolatedPerSourceDir proves entries of two
// different schema dirs never mix, even with identical content.
func TestCRDYAMLToSchemaDir_CacheIsolatedPerSourceDir(t *testing.T) {
	a, b, cache := t.TempDir(), t.TempDir(), t.TempDir()
	writeFile(t, a, "crd.yaml", testCRDYAML)
	writeFile(t, b, "crd.yaml", testCRDYAML)

	outA := convertWithCache(t, a, cache)
	outB := convertWithCache(t, b, cache)
	if outA == outB {
		t.Fatalf("different source dirs must map to different cache dirs, both %q", outA)
	}
	if !fileExists(filepath.Join(outB, "widget-test-v1.json")) {
		t.Error("the second source dir should have its own converted schema")
	}
}
