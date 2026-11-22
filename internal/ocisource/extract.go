package ocisource

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// Extraction limits for untrusted artifact content: a manifest URL is
// attacker-controlled input, so a decompression bomb or a huge file tree
// must fail the fetch instead of filling the disk.
const (
	// maxExtractBytes caps the total uncompressed size of one artifact.
	maxExtractBytes = 512 << 20
	// maxExtractFiles caps the number of regular files in one artifact.
	maxExtractFiles = 100_000
)

// extractTarGz extracts the gzip-compressed tar stream r into dst,
// verifying the compressed bytes against layerDesc.Digest as they stream
// through (the digest covers the blob as stored — before gunzip). The tar
// is untrusted: entry names must be relative and stay inside dst (enforced
// by writing through an os.Root — the kernel rejects escapes), absolute
// paths and ".." are rejected outright, and symlinks/hardlinks are skipped
// (Flux push artifacts do not carry them; a link escaping the extraction
// root must never materialize). Non-regular, non-directory entry types
// (devices, fifos) fail the fetch.
func extractTarGz(r io.Reader, layerDesc ocispec.Descriptor, dst string) error {
	dig := layerDesc.Digest
	verifier := dig.Verifier()
	gz, err := gzip.NewReader(io.TeeReader(r, verifier))
	if err != nil {
		return fmt.Errorf("layer is not valid gzip: %w", err)
	}
	defer gz.Close()

	root, err := os.OpenRoot(dst)
	if err != nil {
		return fmt.Errorf("opening extraction root: %w", err)
	}
	defer root.Close()

	tr := tar.NewReader(gz)
	var budget extractBudget
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading archive: %w", err)
		}
		written, isFile, err := extractEntry(root, tr, hdr)
		if err != nil {
			return err
		}
		if err := budget.add(written, isFile); err != nil {
			return err
		}
	}
	if !verifier.Verified() {
		return fmt.Errorf("layer content does not match %s", dig)
	}
	return nil
}

// extractBudget enforces the extraction caps across entries: files and
// total bytes of regular files only — directories cost nothing.
type extractBudget struct {
	total int64
	files int
}

// add accounts one entry against the budget, failing once a cap is
// exceeded.
func (b *extractBudget) add(written int64, isFile bool) error {
	if !isFile {
		return nil
	}
	b.files++
	if b.files > maxExtractFiles {
		return fmt.Errorf("archive holds more than %d files", maxExtractFiles)
	}
	b.total += written
	if b.total > maxExtractBytes {
		return fmt.Errorf("archive expands beyond %d bytes", int64(maxExtractBytes))
	}
	return nil
}

// extractEntry materializes one tar entry under root and reports how many
// bytes were written and whether the entry was a regular file (the two
// extraction limits only count files). Metadata entries (pax global
// headers) and links write nothing and succeed — per-file pax records are
// applied by archive/tar transparently, links are skipped by policy (see
// extractTarGz).
func extractEntry(root *os.Root, tr *tar.Reader, hdr *tar.Header) (written int64, isFile bool, err error) {
	name, err := safeEntryName(hdr.Name)
	if err != nil {
		return 0, false, err
	}
	if name == "" || name == "." {
		return 0, false, nil
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err := root.MkdirAll(name, 0o755); err != nil {
			return 0, false, fmt.Errorf("creating directory %q: %w", hdr.Name, err)
		}
		return 0, false, nil
	case tar.TypeReg:
		n, err := writeEntryFile(root, tr, hdr, name)
		return n, true, err
	case tar.TypeSymlink, tar.TypeLink, tar.TypeXGlobalHeader:
		return 0, false, nil
	default:
		return 0, false, fmt.Errorf("unsupported tar entry %q of type %q", hdr.Name, string(rune(hdr.Typeflag)))
	}
}

// writeEntryFile writes one regular file entry under root.
func writeEntryFile(root *os.Root, tr *tar.Reader, hdr *tar.Header, name string) (int64, error) {
	if hdr.Size < 0 {
		return 0, fmt.Errorf("entry %q declares a negative size", hdr.Name)
	}
	if err := root.MkdirAll(path.Dir(name), 0o755); err != nil {
		return 0, fmt.Errorf("creating directory for %q: %w", hdr.Name, err)
	}
	mode := os.FileMode(hdr.Mode).Perm()
	if mode == 0 {
		mode = 0o644
	}
	out, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return 0, fmt.Errorf("creating file %q: %w", hdr.Name, err)
	}
	// io.CopyN enforces the declared entry size: a stream shorter than
	// promised fails instead of silently truncating.
	n, err := io.CopyN(out, tr, hdr.Size)
	if err != nil {
		out.Close()
		return 0, fmt.Errorf("writing file %q: %w", hdr.Name, err)
	}
	if err := out.Close(); err != nil {
		return 0, fmt.Errorf("closing file %q: %w", hdr.Name, err)
	}
	return n, nil
}

// safeEntryName validates one tar entry name: it must be relative, free
// of ".." components and backslashes, and clean to a path staying inside
// the extraction root.
func safeEntryName(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if strings.Contains(name, "\\") {
		return "", fmt.Errorf("archive entry %q uses a backslash path separator", name)
	}
	clean := path.Clean(strings.TrimSuffix(name, "/"))
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("archive entry %q escapes the extraction root", name)
	}
	return clean, nil
}
