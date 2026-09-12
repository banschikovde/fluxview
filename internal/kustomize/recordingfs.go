package kustomize

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"

	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// recordingFs wraps the restricted filesystem and records every input a
// kustomize build actually reads, identified by content: successful file
// reads become file records (path, size, sha256) and confirmed directories
// become dir records (path, sorted entry names). The records form the input
// manifest that lets the build cache (see buildcache.go) verify a cached
// output is still fresh:
//
//   - a file's content changing invalidates the entry;
//   - a directory's listing changing (file added/removed/renamed inside it)
//     invalidates the entry, which is what catches new files pulled in by
//     glob or directory resources without touching any recorded file.
//
// Content (not mtime) is the identity, so entries survive fresh git
// checkouts — the property that makes the cache useful in CI.
//
// Empirically kustomize builds only use ReadFile and CleanedAbs (plus Open,
// ReadDir, Glob and Walk for other shapes of repositories); all six are
// intercepted so a future kustomize version that switches call patterns is
// still recorded correctly. For reads that pass through this layer — ReadFile
// directly, Open through a hashing wrapper once EOF is reached — the hash is
// computed from the very bytes served to kustomize, so the manifest always
// describes the content the build actually consumed; records that never saw
// bytes (Glob/Walk, or an Open closed before EOF) are hashed from disk when
// the manifest is snapshotted.
//
// If a recorded path cannot be hashed or listed at snapshot time, the
// recording is poisoned: the build result is real, but the cache must not
// store an entry whose inputs cannot be fully verified later.
type recordingFs struct {
	filesys.FileSystem

	mu       sync.Mutex
	files    map[string]fileRecord
	dirs     map[string]bool
	poisoned bool
}

// fileRecord is one recorded input file. hash is empty until known (filled
// from the served bytes at read time or from disk at snapshot time).
type fileRecord struct {
	size int64
	hash string
}

// newRecordingFs creates a recording layer around inner. The recording is
// safe for concurrent use even though builds are typically sequential.
func newRecordingFs(inner filesys.FileSystem) *recordingFs {
	return &recordingFs{
		FileSystem: inner,
		files:      make(map[string]fileRecord),
		dirs:       make(map[string]bool),
	}
}

// ReadFile records the path and the hash of the served bytes after a
// successful read.
func (fs *recordingFs) ReadFile(path string) ([]byte, error) {
	data, err := fs.FileSystem.ReadFile(path)
	if err == nil {
		sum := sha256.Sum256(data)
		fs.rememberFile(path, int64(len(data)), hex.EncodeToString(sum[:]))
	}
	return data, err
}

// Open records the path and returns a wrapper that hashes the file as
// kustomize reads it: once EOF is reached, the record carries the sha256 of
// the bytes the build actually consumed — the same read/record race closure
// ReadFile has. Files closed before EOF keep the stat-only record and are
// hashed from disk at snapshot time.
func (fs *recordingFs) Open(path string) (filesys.File, error) {
	f, err := fs.FileSystem.Open(path)
	if err == nil {
		fs.rememberFile(path, -1, "")
		return &hashingFile{File: f, recorder: fs, path: normalizeFsPath(path), hash: sha256.New()}, nil
	}
	return f, err
}

// hashingFile wraps a filesys.File and feeds everything read through it into
// a sha256, finalizing the recordingFs record when the stream reaches EOF.
type hashingFile struct {
	filesys.File
	recorder *recordingFs
	path     string
	hash     hash.Hash
	size     int64
	done     bool
}

func (f *hashingFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	if n > 0 {
		f.hash.Write(p[:n])
		f.size += int64(n)
	}
	if err == io.EOF && !f.done {
		f.done = true
		sum := f.hash.Sum(nil)
		f.recorder.rememberFile(f.path, f.size, hex.EncodeToString(sum))
	}
	return n, err
}

// ReadDir records the directory after a successful listing.
func (fs *recordingFs) ReadDir(path string) ([]string, error) {
	entries, err := fs.FileSystem.ReadDir(path)
	if err == nil {
		fs.rememberDir(path)
	}
	return entries, err
}

// Glob records every match plus the pattern's static directory prefix, so a
// file newly matching the pattern (new file in the directory) invalidates.
func (fs *recordingFs) Glob(pattern string) ([]string, error) {
	matches, err := fs.FileSystem.Glob(pattern)
	if err == nil {
		for _, m := range matches {
			if info, statErr := os.Stat(m); statErr == nil {
				if info.IsDir() {
					fs.rememberDir(m)
				} else {
					fs.rememberFile(m, info.Size(), "")
				}
			}
		}
		if root := globStaticRoot(pattern); root != "" {
			fs.rememberDir(root)
		}
	}
	return matches, err
}

// Walk records the walked root and every entry visited.
func (fs *recordingFs) Walk(path string, walkFn filepath.WalkFunc) error {
	return fs.FileSystem.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil {
			if info.IsDir() {
				fs.rememberDir(p)
			} else {
				fs.rememberFile(p, info.Size(), "")
			}
		}
		return walkFn(p, info, err)
	})
}

// CleanedAbs records the confirmed directory after a successful call. This is
// how kustomization roots get their directory record.
func (fs *recordingFs) CleanedAbs(path string) (filesys.ConfirmedDir, string, error) {
	dir, file, err := fs.FileSystem.CleanedAbs(path)
	if err == nil && dir != "" {
		fs.rememberDir(string(dir))
	}
	return dir, file, err
}

// rememberFile stores or refines a file record; a later record with a known
// hash upgrades an earlier stat-only one.
func (fs *recordingFs) rememberFile(path string, size int64, hash string) {
	norm := normalizeFsPath(path)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	existing, ok := fs.files[norm]
	switch {
	case !ok:
		fs.files[norm] = fileRecord{size: size, hash: hash}
	case hash != "" && existing.hash == "":
		// Upgrade a stat-only record with the content hash.
		if size >= 0 {
			existing.size = size
		}
		existing.hash = hash
		fs.files[norm] = existing
	}
}

// rememberDir marks a directory as recorded.
func (fs *recordingFs) rememberDir(path string) {
	norm := normalizeFsPath(path)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.dirs[norm] = true
}

// manifest snapshots the recorded inputs with paths relative to rootDir where
// possible (paths outside rootDir, e.g. remote-cache files, stay absolute).
// File records without a hash and all directory listings are completed from
// disk here; any failure poisons the snapshot. The slices are sorted for
// deterministic cache entries.
func (fs *recordingFs) manifest(rootDir string) (files []buildFileInput, dirs []buildDirInput, ok bool) {
	fs.mu.Lock()
	poisoned := fs.poisoned
	paths := make([]string, 0, len(fs.files))
	for p := range fs.files {
		paths = append(paths, p)
	}
	dirPaths := make([]string, 0, len(fs.dirs))
	for p := range fs.dirs {
		dirPaths = append(dirPaths, p)
	}
	fs.mu.Unlock()

	if poisoned {
		return nil, nil, false
	}

	files = make([]buildFileInput, 0, len(paths))
	for _, p := range paths {
		fs.mu.Lock()
		rec := fs.files[p]
		fs.mu.Unlock()
		if rec.hash == "" {
			hash, size, err := hashFileContent(p)
			if err != nil {
				fs.poison()
				return nil, nil, false
			}
			rec.hash, rec.size = hash, size
		}
		files = append(files, buildFileInput{
			Path:   cachePathRelative(rootDir, p),
			Size:   rec.size,
			SHA256: rec.hash,
		})
	}

	dirs = make([]buildDirInput, 0, len(dirPaths))
	for _, p := range dirPaths {
		entries := listDirEntries(p)
		if entries == nil {
			fs.poison()
			return nil, nil, false
		}
		dirs = append(dirs, buildDirInput{
			Path:    cachePathRelative(rootDir, p),
			Entries: entries,
		})
	}
	sortInputs(files, dirs)
	return files, dirs, true
}

// poison marks the recording unusable for caching.
func (fs *recordingFs) poison() {
	fs.mu.Lock()
	fs.poisoned = true
	fs.mu.Unlock()
}

// globStaticRoot returns the directory part of a glob pattern before the
// first metacharacter ("a/b/*.yaml" → "a/b"), or "" when the pattern starts
// with a metacharacter or has no directory part.
func globStaticRoot(pattern string) string {
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*', '?', '[', '{':
			dir := filepath.Dir(pattern[:i])
			if dir == "." {
				return ""
			}
			return dir
		}
	}
	return ""
}
