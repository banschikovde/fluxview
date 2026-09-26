package yamlutil

import (
	"reflect"
	"testing"
)

func TestMergeMaps(t *testing.T) {
	t.Run("nested maps merge recursively", func(t *testing.T) {
		dst := map[string]any{
			"config": map[string]any{"a": "from-cm", "b": "from-cm"},
			"keep":   "dst",
		}
		MergeMaps(dst, map[string]any{
			"config": map[string]any{"b": "from-inline"},
			"new":    "src",
		})
		want := map[string]any{
			"config": map[string]any{"a": "from-cm", "b": "from-inline"},
			"keep":   "dst",
			"new":    "src",
		}
		if !reflect.DeepEqual(dst, want) {
			t.Errorf("merge = %#v, want %#v", dst, want)
		}
	})

	t.Run("scalars and lists overwrite", func(t *testing.T) {
		dst := map[string]any{"replicas": 1, "tolerations": []any{"a"}, "name": "old"}
		MergeMaps(dst, map[string]any{"replicas": 3, "tolerations": []any{"b"}, "name": nil})
		want := map[string]any{"replicas": 3, "tolerations": []any{"b"}, "name": nil}
		if !reflect.DeepEqual(dst, want) {
			t.Errorf("merge = %#v, want %#v", dst, want)
		}
	})

	t.Run("map over scalar and scalar over map overwrite", func(t *testing.T) {
		dst := map[string]any{"x": "scalar", "y": map[string]any{"k": 1}}
		MergeMaps(dst, map[string]any{"x": map[string]any{"n": 2}, "y": "scalar"})
		want := map[string]any{"x": map[string]any{"n": 2}, "y": "scalar"}
		if !reflect.DeepEqual(dst, want) {
			t.Errorf("merge = %#v, want %#v", dst, want)
		}
	})

	t.Run("nil values do not break the merge", func(t *testing.T) {
		dst := map[string]any{"a": map[string]any{"b": 1}}
		MergeMaps(dst, map[string]any{"a": nil, "c": nil})
		want := map[string]any{"a": nil, "c": nil}
		if !reflect.DeepEqual(dst, want) {
			t.Errorf("merge = %#v, want %#v", dst, want)
		}
	})

	t.Run("empty src is a no-op", func(t *testing.T) {
		dst := map[string]any{"a": 1}
		MergeMaps(dst, nil)
		if !reflect.DeepEqual(dst, map[string]any{"a": 1}) {
			t.Errorf("merge with nil src = %#v", dst)
		}
	})
}
