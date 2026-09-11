package kustomize

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// rewritingFs intercepts reads of kustomization files and serves a rewritten
// copy in which every remote resource URL resolved by the remote cache is
// replaced with its absolute cache path. All other reads pass through to the
// wrapped (restricted) filesystem unchanged.
//
// This is the substitution half of the remote cache: kustomize itself is never
// modified or forked — it simply never encounters a URL, because the bytes it
// reads already point at local files. Rewriting happens at the read boundary
// rather than by copying kustomization files to a snapshot directory, so
// builds keep running against real repository paths (relative ../../ bases,
// symlinks and the restrictedFs security boundary all behave exactly as
// before).
//
// Results are memoized per path: each kustomization file is parsed at most
// once per Builder, and repeated reads (kustomize reads files more than once)
// are served from memory.
type rewritingFs struct {
	filesys.FileSystem // the restricted filesystem being wrapped
	cache              *remoteCache

	mu   sync.Mutex
	memo map[string]rewrittenFile // normalized path → result
}

// rewrittenFile is the memoized outcome of one rewrite attempt; changed=false
// means "serve the original bytes from disk".
type rewrittenFile struct {
	changed bool
	data    []byte
}

// wrapFs layers URL rewriting on top of a restricted filesystem.
func (c *remoteCache) wrapFs(inner filesys.FileSystem) filesys.FileSystem {
	return &rewritingFs{FileSystem: inner, cache: c, memo: make(map[string]rewrittenFile)}
}

func (fs *rewritingFs) ReadFile(path string) ([]byte, error) {
	if entry, ok := fs.memoized(path); ok {
		if entry.changed {
			return entry.data, nil
		}
		return fs.FileSystem.ReadFile(path)
	}
	data, err := fs.FileSystem.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rewritten, changed := fs.rewriteIfKustomization(path, data)
	fs.remember(path, changed, rewritten)
	if changed {
		return rewritten, nil
	}
	return data, nil
}

func (fs *rewritingFs) Open(path string) (filesys.File, error) {
	// Only kustomization files can be rewritten — skip everything else before
	// any I/O so non-kustomization files are opened exactly once, by the
	// wrapped filesystem.
	if !isKustomizationFileName(filepath.Base(path)) {
		return fs.FileSystem.Open(path)
	}
	if entry, ok := fs.memoized(path); ok {
		if entry.changed {
			return newMemFile(entry.data), nil
		}
		return fs.FileSystem.Open(path)
	}
	// Kustomization files are small; reading the full content here (instead
	// of streaming) is required — the rewrite needs the whole document.
	data, err := fs.FileSystem.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rewritten, changed := fs.rewriteIfKustomization(path, data)
	fs.remember(path, changed, rewritten)
	if changed {
		return newMemFile(rewritten), nil
	}
	// Unchanged: hand back the real file to preserve streaming semantics.
	return fs.FileSystem.Open(path)
}

// memoized returns the memo entry for path, if present.
func (fs *rewritingFs) memoized(path string) (rewrittenFile, bool) {
	key := normalizeFsPath(path)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	entry, ok := fs.memo[key]
	return entry, ok
}

// remember stores a rewrite outcome. Failures are not memoized (there are
// none — rewriteIfKustomization falls back to "unchanged" on any parse error),
// so only successful reads reach this.
func (fs *rewritingFs) remember(path string, changed bool, data []byte) {
	key := normalizeFsPath(path)
	fs.mu.Lock()
	fs.memo[key] = rewrittenFile{changed: changed, data: data}
	fs.mu.Unlock()
}

// rewriteIfKustomization rewrites remote resource URLs in a kustomization
// file's content. Non-kustomization files, files without remote refs, and
// files whose URLs are not resolved by the cache are returned unchanged.
// The YAML round-trip goes through yaml.v3's Node representation, preserving
// key order, comments and formatting — only the substituted scalars change.
func (fs *rewritingFs) rewriteIfKustomization(path string, data []byte) ([]byte, bool) {
	if !isKustomizationFileName(filepath.Base(path)) {
		return data, false
	}
	// Cheap prefilter: no "http" substring anywhere — nothing to rewrite.
	if !bytes.Contains(data, []byte("http")) {
		return data, false
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return data, false // kustomize will surface the parse error identically
	}
	root := &node
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return data, false
	}

	changed := false
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if key.Kind != yaml.ScalarNode || value.Kind != yaml.SequenceNode {
			continue
		}
		if key.Value != "resources" && key.Value != "components" {
			continue
		}
		for _, item := range value.Content {
			if item.Kind != yaml.ScalarNode || !isHTTPRef(item.Value) {
				continue
			}
			if cachePath, ok := fs.cache.resolve(item.Value); ok {
				item.Value = cachePath
				item.Tag = "!!str"
				item.Style = 0
				changed = true
			}
		}
	}
	if !changed {
		return data, false
	}
	out, err := yaml.Marshal(&node)
	if err != nil {
		return data, false
	}
	return out, true
}

// normalizeFsPath produces a stable memo key: absolute and symlink-resolved,
// matching the normalization restrictedFs applies on every access (e.g. macOS
// /var → /private/var).
func normalizeFsPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// memFile adapts in-memory bytes to kyaml's filesys.File (io.ReadWriteCloser +
// Stat) so rewritten kustomization content can be served through Open.
type memFile struct {
	*bytes.Reader
	name string
}

func newMemFile(data []byte) *memFile {
	return &memFile{Reader: bytes.NewReader(data), name: "memfile"}
}

func (m *memFile) Stat() (os.FileInfo, error) {
	return memFileInfo{name: m.name, size: int64(m.Len())}, nil
}
func (m *memFile) Close() error { return nil }
func (m *memFile) Write(p []byte) (int, error) {
	return 0, fmt.Errorf("file %s is read-only", m.name)
}

// memFileInfo is the minimal os.FileInfo for memFile.
type memFileInfo struct {
	name string
	size int64
}

func (i memFileInfo) Name() string       { return i.name }
func (i memFileInfo) Size() int64        { return i.size }
func (i memFileInfo) Mode() os.FileMode  { return 0o444 }
func (i memFileInfo) ModTime() time.Time { return time.Time{} }
func (i memFileInfo) IsDir() bool        { return false }
func (i memFileInfo) Sys() any           { return nil }
