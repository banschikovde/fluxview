// Package ocisourcetest provides a local OCI registry for tests: a real
// in-process distribution registry with htpasswd auth on a loopback plain
// HTTP port (helm's repotest server), plus helpers to push kustomize-style
// artifacts (arbitrary file trees as tar+gzip layers) into it.
package ocisourcetest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"helm.sh/helm/v4/pkg/repo/v1/repotest"
)

// DefaultLayerMediaType is a kustomize-artifact-style layer media type for
// tests that do not exercise layer selection.
const DefaultLayerMediaType = "application/vnd.fluxcd.source.v1.tar+gzip"

// Registry is a local htpasswd-protected OCI registry. The server stops
// with the test's context.
type Registry struct {
	// URL is the registry base (host:port, no scheme, no oci:// prefix).
	URL string
	// Username and Password authenticate against the registry.
	Username string
	Password string
}

// dummyConfiguredCredentialStore is a docker/registry config that counts
// as "configured" (a dummy auths entry) while holding no credentials.
const dummyConfiguredCredentialStore = `{"auths": {"fluxview-test.invalid": {}}}` + "\n"

// IsolateCredentialStores points DOCKER_CONFIG and HELM_CONFIG_HOME at
// per-test directories holding configured-but-empty credential configs.
// oras (and Helm on top of it) only fall back to the platform's native
// credential store — the macOS keychain, the Linux helper binaries —
// when the config carries no authentication at all; a configured config
// pins every read and write to these temp files. Tests neither depend on
// the machine's keychain nor write into it (Helm's repotest login used
// to leave real entries there).
func IsolateCredentialStores(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"docker", "helm/registry"} {
		d := filepath.Join(dir, sub)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("isolating credential stores: %v", err)
		}
		if err := os.WriteFile(filepath.Join(d, "config.json"), []byte(dummyConfiguredCredentialStore), 0o600); err != nil {
			t.Fatalf("isolating credential stores: %v", err)
		}
	}
	t.Setenv("DOCKER_CONFIG", filepath.Join(dir, "docker"))
	t.Setenv("HELM_CONFIG_HOME", filepath.Join(dir, "helm"))
}

// NewRegistry starts the local registry under isolated credential stores.
func NewRegistry(t *testing.T) *Registry {
	t.Helper()
	IsolateCredentialStores(t)
	srv, err := repotest.NewOCIServer(t, t.TempDir())
	if err != nil {
		t.Fatalf("starting test OCI registry: %v", err)
	}
	go srv.ListenAndServe()
	return &Registry{URL: srv.RegistryURL, Username: srv.TestUsername, Password: srv.TestPassword}
}

// Artifact describes one pushed artifact version.
type Artifact struct {
	// Ref is the full repository reference with tag ("host:port/repo:tag").
	Ref string
	// Digest is the manifest digest ("sha256:...").
	Digest string
	// Size is the manifest size in bytes.
	Size int64
}

// LayerSpec is one artifact layer: a file tree packed as tar+gzip under
// the given media type.
type LayerSpec struct {
	MediaType string
	Files     map[string]string
}

// PushArtifact packs files (name → content) as a tar+gzip layer with the
// given media type and pushes it as an artifact tagged tag.
func (r *Registry) PushArtifact(t *testing.T, repo, tag, layerMediaType string, files map[string]string) Artifact {
	t.Helper()
	return r.PushLayeredArtifact(t, repo, tag, LayerSpec{MediaType: layerMediaType, Files: files})
}

// PushLayerBytes pushes an arbitrary blob as the single layer of an
// artifact tagged tag — for malformed-content tests.
func (r *Registry) PushLayerBytes(t *testing.T, repo, tag, layerMediaType string, blob []byte) Artifact {
	t.Helper()
	return r.push(t, repo, tag, func(store *memory.Store) (ocispec.Descriptor, error) {
		ctx := context.Background()
		layerDesc, err := oras.PushBytes(ctx, store, layerMediaType, blob)
		if err != nil {
			return ocispec.Descriptor{}, err
		}
		return oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, "application/vnd.fluxcd.source.v1", oras.PackManifestOptions{
			Layers: []ocispec.Descriptor{layerDesc},
		})
	})
}

// PushLayeredArtifact pushes an artifact with several layers in order —
// for layerSelector tests.
func (r *Registry) PushLayeredArtifact(t *testing.T, repo, tag string, layers ...LayerSpec) Artifact {
	t.Helper()
	return r.push(t, repo, tag, func(store *memory.Store) (ocispec.Descriptor, error) {
		ctx := context.Background()
		var descs []ocispec.Descriptor
		for _, l := range layers {
			blob, err := tarGz(l.Files)
			if err != nil {
				return ocispec.Descriptor{}, err
			}
			desc, err := oras.PushBytes(ctx, store, l.MediaType, blob)
			if err != nil {
				return ocispec.Descriptor{}, err
			}
			descs = append(descs, desc)
		}
		return oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, "application/vnd.fluxcd.source.v1", oras.PackManifestOptions{
			Layers: descs,
		})
	})
}

// PushRawManifest pushes arbitrary bytes as the tagged manifest without
// following successors — for malformed-manifest tests (an unsupported
// index, a layerless manifest).
func (r *Registry) PushRawManifest(t *testing.T, repo, tag, mediaType string, manifest []byte) Artifact {
	t.Helper()
	opts := oras.DefaultCopyOptions
	// No successor traversal: the raw bytes may reference blobs that do
	// not exist anywhere (a malformed-manifest fixture).
	opts.FindSuccessors = func(context.Context, content.Fetcher, ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		return nil, nil
	}
	return r.pushWithOptions(t, repo, tag, opts, func(store *memory.Store) (ocispec.Descriptor, error) {
		return oras.PushBytes(context.Background(), store, mediaType, manifest)
	})
}

// PushBlob pushes an arbitrary blob into the repository (no manifest
// wrapping, no successor traversal).
func (r *Registry) PushBlob(t *testing.T, repo, mediaType string, blob []byte) {
	t.Helper()
	opts := oras.DefaultCopyOptions
	opts.FindSuccessors = func(context.Context, content.Fetcher, ocispec.Descriptor) ([]ocispec.Descriptor, error) {
		return nil, nil
	}
	store := memory.New()
	desc, err := oras.PushBytes(context.Background(), store, mediaType, blob)
	if err != nil {
		t.Fatalf("pushing blob: %v", err)
	}
	dst := r.destination(t, repo)
	// CopyGraph moves the blob itself (Copy would route a tagged root
	// through the manifest endpoint).
	if err := oras.CopyGraph(context.Background(), store, dst, desc, opts.CopyGraphOptions); err != nil {
		t.Fatalf("copying blob: %v", err)
	}
}

// PushLayerlessManifest pushes a manifest with zero layers; the canonical
// empty config blob is pushed first so the registry accepts it.
func (r *Registry) PushLayerlessManifest(t *testing.T, repo, tag string) Artifact {
	t.Helper()
	r.PushBlob(t, repo, "application/vnd.oci.empty.v1+json", []byte("{}"))
	empty := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":"sha256:` + emptyConfigSHA + `","size":2},"layers":[]}`
	return r.PushRawManifest(t, repo, tag, "application/vnd.oci.image.manifest.v1+json", []byte(empty))
}

// emptyConfigSHA is sha256("{}") — the canonical empty config blob.
const emptyConfigSHA = "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"

// PushIndex pushes a multi-platform index manifest over a real artifact
// of the same repository — registries validate that index entries exist.
func (r *Registry) PushIndex(t *testing.T, repo, tag string) Artifact {
	t.Helper()
	child := r.PushArtifact(t, repo, tag+"-child", DefaultLayerMediaType, map[string]string{"f.txt": "x\n"})
	idx := ocispec.Index{
		Versioned: struct {
			SchemaVersion int `json:"schemaVersion"`
		}{SchemaVersion: 2},
		MediaType: ocispec.MediaTypeImageIndex,
		Manifests: []ocispec.Descriptor{{
			MediaType:   ocispec.MediaTypeImageManifest,
			Digest:      digest.Digest(child.Digest),
			Size:        child.Size,
			Annotations: map[string]string{"platform": "none"},
		}},
	}
	data, err := json.Marshal(idx)
	if err != nil {
		t.Fatalf("marshalling index: %v", err)
	}
	return r.PushRawManifest(t, repo, tag, ocispec.MediaTypeImageIndex, data)
}

// push runs pack and copies with default options.
func (r *Registry) push(t *testing.T, repo, tag string, pack func(store *memory.Store) (ocispec.Descriptor, error)) Artifact {
	t.Helper()
	return r.pushWithOptions(t, repo, tag, oras.DefaultCopyOptions, pack)
}

// destination builds the authenticated plain-HTTP remote handle for
// repo under this test registry.
func (r *Registry) destination(t *testing.T, repo string) *remote.Repository {
	t.Helper()
	dst, err := remote.NewRepository(r.URL + "/" + repo)
	if err != nil {
		t.Fatalf("parsing destination reference: %v", err)
	}
	dst.PlainHTTP = true
	dst.Client = &auth.Client{
		Client: retry.DefaultClient,
		Credential: auth.StaticCredential(r.URL, auth.Credential{
			Username: r.Username,
			Password: r.Password,
		}),
	}
	return dst
}

// pushWithOptions runs pack (building the manifest in the memory store),
// tags it and copies it into the registry with the given copy options.
func (r *Registry) pushWithOptions(t *testing.T, repo, tag string, opts oras.CopyOptions, pack func(store *memory.Store) (ocispec.Descriptor, error)) Artifact {
	t.Helper()

	ctx := context.Background()
	store := memory.New()
	manifestDesc, err := pack(store)
	if err != nil {
		t.Fatalf("packing artifact: %v", err)
	}
	if err := store.Tag(ctx, manifestDesc, tag); err != nil {
		t.Fatalf("tagging artifact: %v", err)
	}

	dst := r.destination(t, repo)
	if _, err := oras.Copy(ctx, store, tag, dst, tag, opts); err != nil {
		t.Fatalf("pushing artifact: %v", err)
	}
	return Artifact{
		Ref:    dst.Reference.Registry + "/" + dst.Reference.Repository + ":" + tag,
		Digest: manifestDesc.Digest.String(),
		Size:   manifestDesc.Size,
	}
}

// tarGz packs files (name → content) into an in-memory tar+gzip archive.
func tarGz(files map[string]string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}); err != nil {
			return nil, fmt.Errorf("writing header %s: %w", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			return nil, fmt.Errorf("writing %s: %w", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
