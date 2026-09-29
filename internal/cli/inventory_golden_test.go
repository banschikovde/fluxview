package cli

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/banschikovde/fluxview/internal/inventory"
)

// update regenerates the golden files: go test ./internal/cli/ -run TestInventoryGolden -update
var update = flag.Bool("update", false, "rewrite golden files")

// setupInventoryGoldenRepo copies the testdata/inventory fixture into a
// fresh git repository (the fixture is plain files; the pipeline needs a
// repo root) and returns its path.
func setupInventoryGoldenRepo(t *testing.T) string {
	t.Helper()
	src := filepath.Join("testdata", "inventory")
	dst := t.TempDir()

	// Copy the fixture tree.
	if err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	}); err != nil {
		t.Fatalf("copying fixture: %v", err)
	}

	gitInit(t, dst)
	return dst
}

// TestInventoryGolden runs the inventory command over the testdata fixture
// and compares stdout byte-for-byte with the golden files (deterministic
// output).
func TestInventoryGolden(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*InventoryFlags)
	}{
		{"table", func(f *InventoryFlags) {}},
		{"markdown", func(f *InventoryFlags) { f.Output = "markdown" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot := setupInventoryGoldenRepo(t)

			flags := &InventoryFlags{
				Path:   filepath.Join(repoRoot, "clusters", "test"),
				Output: "table",
				Sort:   inventory.DefaultSort,
			}
			tc.mutate(flags)

			var err error
			output := captureStdout(func() {
				err = runInventory(context.Background(), flags)
			})
			if err != nil {
				t.Fatalf("runInventory: %v", err)
			}

			goldenPath := filepath.Join("testdata", "golden", "inventory-"+tc.name+".txt")
			if *update {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0755); err != nil {
					t.Fatalf("mkdir golden dir: %v", err)
				}
				if err := os.WriteFile(goldenPath, []byte(output), 0644); err != nil {
					t.Fatalf("writing golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("reading golden (run with -update to create): %v", err)
			}
			if output != string(want) {
				t.Errorf("output differs from golden %s:\n--- got ---\n%s\n--- want ---\n%s",
					goldenPath, output, want)
			}
		})
	}
}
