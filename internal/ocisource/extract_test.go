package ocisource

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// tarGzBytes builds an in-memory tar+gzip archive; hdrs allows crafting
// entries the safe path would never produce (absolute names, links,
// devices).
func tarGzBytes(t *testing.T, hdrs []*tar.Header, contents map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, hdr := range hdrs {
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("header %q: %v", hdr.Name, err)
		}
		if c, ok := contents[hdr.Name]; ok && hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(c)); err != nil {
				t.Fatalf("write %q: %v", hdr.Name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func regHdr(name string, size int64) *tar.Header {
	return &tar.Header{Name: name, Mode: 0o644, Size: size, Typeflag: tar.TypeReg}
}

// descFor returns a descriptor whose digest matches content (so the
// extraction verification passes), or a mismatching one.
func descFor(content []byte, mismatch bool) ocispec.Descriptor {
	d := digest.FromBytes(content)
	if mismatch {
		d = digest.FromString("a deliberately wrong digest")
	}
	return ocispec.Descriptor{MediaType: DefaultLayerMediaTypeTest, Digest: d, Size: int64(len(content))}
}

// DefaultLayerMediaTypeTest mirrors the test helper constant without an
// import cycle.
const DefaultLayerMediaTypeTest = "application/vnd.fluxcd.source.v1.tar+gzip"

func extract(t *testing.T, blob []byte, mismatch bool) error {
	t.Helper()
	dst := t.TempDir()
	return extractTarGz(bytes.NewReader(blob), descFor(blob, mismatch), dst)
}

func TestExtractTarGz_FilesAndDirs(t *testing.T) {
	content := map[string]string{"dir/sub/f.txt": "hello\n"}
	hdrs := []*tar.Header{
		{Name: "dir/", Mode: 0o755, Typeflag: tar.TypeDir},
		{Name: "dir/sub/", Mode: 0o755, Typeflag: tar.TypeDir},
		regHdr("dir/sub/f.txt", 6),
	}
	blob := tarGzBytes(t, hdrs, content)
	dst := t.TempDir()
	if err := extractTarGz(bytes.NewReader(blob), descFor(blob, false), dst); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "dir", "sub", "f.txt"))
	if err != nil || string(got) != "hello\n" {
		t.Fatalf("extracted content mismatch: %q err=%v", got, err)
	}
}

func TestExtractTarGz_LinksSkipped(t *testing.T) {
	hdrs := []*tar.Header{
		regHdr("real.txt", 3),
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "real.txt"},
		{Name: "hard", Typeflag: tar.TypeLink, Linkname: "real.txt"},
	}
	blob := tarGzBytes(t, hdrs, map[string]string{"real.txt": "abc"})
	dst := t.TempDir()
	if err := extractTarGz(bytes.NewReader(blob), descFor(blob, false), dst); err != nil {
		t.Fatalf("links must be skipped, not fail: %v", err)
	}
	for _, link := range []string{"link", "hard"} {
		if _, err := os.Lstat(filepath.Join(dst, link)); err == nil {
			t.Errorf("%s must not materialize", link)
		}
	}
}

func TestExtractTarGz_UnsupportedEntryTypeFails(t *testing.T) {
	hdrs := []*tar.Header{{Name: "dev", Typeflag: tar.TypeBlock}}
	blob := tarGzBytes(t, hdrs, nil)
	err := extract(t, blob, false)
	if err == nil || !strings.Contains(err.Error(), "unsupported tar entry") {
		t.Fatalf("device entries must fail, got %v", err)
	}
}

func TestExtractTarGz_DigestMismatchFails(t *testing.T) {
	blob := tarGzBytes(t, []*tar.Header{regHdr("f.txt", 3)}, map[string]string{"f.txt": "abc"})
	err := extract(t, blob, true)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("a digest mismatch must fail, got %v", err)
	}
}

func TestExtractTarGz_NotGzipFails(t *testing.T) {
	err := extract(t, []byte("plain text, not gzip"), false)
	if err == nil || !strings.Contains(err.Error(), "not valid gzip") {
		t.Fatalf("non-gzip layers must fail, got %v", err)
	}
}

func TestExtractTarGz_BudgetCaps(t *testing.T) {
	origFiles, origBytes := maxExtractFiles, maxExtractBytes
	t.Cleanup(func() {
		maxExtractFiles = origFiles
		maxExtractBytes = origBytes
	})

	// File-count cap.
	maxExtractFiles = 2
	blob := tarGzBytes(t, []*tar.Header{regHdr("a", 1), regHdr("b", 1), regHdr("c", 1)},
		map[string]string{"a": "x", "b": "y", "c": "z"})
	if err := extract(t, blob, false); err == nil || !strings.Contains(err.Error(), "files") {
		t.Fatalf("the file-count cap must fail the extraction, got %v", err)
	}

	// Byte cap.
	maxExtractFiles = origFiles
	maxExtractBytes = 2
	blob = tarGzBytes(t, []*tar.Header{regHdr("big", 6)}, map[string]string{"big": "123456"})
	if err := extract(t, blob, false); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("the byte cap must fail the extraction, got %v", err)
	}
}

func TestWriteEntryFile_NegativeSizeFails(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := writeEntryFile(root, &tar.Reader{}, &tar.Header{Name: "f", Size: -1, Typeflag: tar.TypeReg}, "f"); err == nil {
		t.Fatal("a negative declared size must fail")
	}
}

func TestSafeEntryName(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{name: "plain", in: "a/b.txt", want: "a/b.txt"},
		{name: "trailing slash", in: "dir/", want: "dir"},
		{name: "dot", in: ".", want: "."},
		{name: "empty", in: "", want: ""},
		{name: "dotdot", in: "../evil", wantErr: true},
		{name: "nested dotdot", in: "a/../../evil", wantErr: true},
		{name: "absolute", in: "/etc/passwd", wantErr: true},
		{name: "backslash", in: "a\\b", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := safeEntryName(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("safeEntryName(%q) = %q, want error", tc.in, got)
				}
				if !strings.Contains(err.Error(), "escapes the extraction root") && !strings.Contains(err.Error(), "backslash") {
					t.Errorf("unexpected error text: %v", err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("safeEntryName(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
			}
		})
	}
}
