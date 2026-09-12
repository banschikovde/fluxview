package kustomize

import (
	"os"
	"path/filepath"
	"sync"

	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// recordingFs wraps the restricted filesystem and records every input a
// kustomize build actually reads: successful file reads become file records
// (path, size, mtime) and successful directory confirmations become dir
// records (path, mtime). The records form the input manifest that lets the
// build cache (see buildcache.go) verify a cached output is still fresh:
//
//   - a file's size/mtime changing invalidates the entry;
//   - a directory's mtime changing (file added/removed/renamed inside it)
//     invalidates the entry, which is what catches new files pulled in by
//     glob or directory resources without touching any recorded file.
//
// Empirically kustomize builds only use ReadFile and CleanedAbs (plus Open,
// ReadDir, Glob and Walk for other shapes of repositories); all six are
// intercepted so a future kustomize version that switches call patterns is
// still recorded correctly.
//
// If an operation succeeds but the path cannot be stat'ed afterwards (or a
// directory record points at a non-directory), the recording is poisoned:
// the build result is real, but the cache must not store an entry whose
// inputs cannot be fully verified later.
type recordingFs struct {
	filesys.FileSystem

	mu       sync.Mutex
	files    map[string]buildFileInput
	dirs     map[string]buildDirInput
	poisoned bool
}

// newRecordingFs creates a recording layer around inner. The recording is
// safe for concurrent use even though builds are typically sequential.
func newRecordingFs(inner filesys.FileSystem) *recordingFs {
	return &recordingFs{
		FileSystem: inner,
		files:      make(map[string]buildFileInput),
		dirs:       make(map[string]buildDirInput),
	}
}

// ReadFile records the path after a successful read.
func (fs *recordingFs) ReadFile(path string) ([]byte, error) {
	data, err := fs.FileSystem.ReadFile(path)
	if err == nil {
		fs.recordFile(path)
	}
	return data, err
}

// Open records the path after a successful open.
func (fs *recordingFs) Open(path string) (filesys.File, error) {
	f, err := fs.FileSystem.Open(path)
	if err == nil {
		fs.recordFile(path)
	}
	return f, err
}

// ReadDir records the directory after a successful listing; its mtime covers
// entries appearing or disappearing.
func (fs *recordingFs) ReadDir(path string) ([]string, error) {
	entries, err := fs.FileSystem.ReadDir(path)
	if err == nil {
		fs.recordDir(path)
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
					fs.recordDirFromInfo(m, info)
				} else {
					fs.recordFileFromInfo(m, info)
				}
			}
		}
		if root := globStaticRoot(pattern); root != "" {
			fs.recordDir(root)
		}
	}
	return matches, err
}

// Walk records the walked root and every entry visited, reusing the FileInfo
// the walk already provides (no extra stat calls).
func (fs *recordingFs) Walk(path string, walkFn filepath.WalkFunc) error {
	return fs.FileSystem.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil {
			if info.IsDir() {
				fs.recordDirFromInfo(p, info)
			} else {
				fs.recordFileFromInfo(p, info)
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
		fs.recordDir(string(dir))
	}
	return dir, file, err
}

// recordFile stats and records a file path; repeated paths are no-ops.
func (fs *recordingFs) recordFile(path string) {
	norm := normalizeFsPath(path)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if _, dup := fs.files[norm]; dup {
		return
	}
	info, err := os.Stat(norm)
	if err != nil {
		fs.poisoned = true
		return
	}
	fs.recordFileLocked(norm, info)
}

// recordFileFromInfo records a file using an already obtained FileInfo.
func (fs *recordingFs) recordFileFromInfo(path string, info os.FileInfo) {
	norm := normalizeFsPath(path)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if _, dup := fs.files[norm]; dup {
		return
	}
	fs.recordFileLocked(norm, info)
}

func (fs *recordingFs) recordFileLocked(norm string, info os.FileInfo) {
	if info == nil || info.IsDir() {
		fs.poisoned = true
		return
	}
	fs.files[norm] = buildFileInput{
		Path:        norm,
		Size:        info.Size(),
		ModTimeNano: info.ModTime().UnixNano(),
	}
}

// recordDir stats and records a directory path; repeated paths are no-ops.
func (fs *recordingFs) recordDir(path string) {
	norm := normalizeFsPath(path)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if _, dup := fs.dirs[norm]; dup {
		return
	}
	info, err := os.Stat(norm)
	if err != nil || !info.IsDir() {
		fs.poisoned = true
		return
	}
	fs.recordDirLocked(norm, info)
}

// recordDirFromInfo records a directory using an already obtained FileInfo.
func (fs *recordingFs) recordDirFromInfo(path string, info os.FileInfo) {
	norm := normalizeFsPath(path)
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if _, dup := fs.dirs[norm]; dup {
		return
	}
	fs.recordDirLocked(norm, info)
}

func (fs *recordingFs) recordDirLocked(norm string, info os.FileInfo) {
	if info == nil || !info.IsDir() {
		fs.poisoned = true
		return
	}
	fs.dirs[norm] = buildDirInput{
		Path:        norm,
		ModTimeNano: info.ModTime().UnixNano(),
	}
}

// manifest snapshots the recorded inputs with paths relative to rootDir where
// possible (paths outside rootDir, e.g. remote-cache files, stay absolute).
// The slices are sorted for deterministic cache entries.
func (fs *recordingFs) manifest(rootDir string) (files []buildFileInput, dirs []buildDirInput, ok bool) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.poisoned {
		return nil, nil, false
	}
	files = make([]buildFileInput, 0, len(fs.files))
	for _, f := range fs.files {
		f.Path = cachePathRelative(rootDir, f.Path)
		files = append(files, f)
	}
	dirs = make([]buildDirInput, 0, len(fs.dirs))
	for _, d := range fs.dirs {
		d.Path = cachePathRelative(rootDir, d.Path)
		dirs = append(dirs, d)
	}
	sortInputs(files, dirs)
	return files, dirs, true
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
