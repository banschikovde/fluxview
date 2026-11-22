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
	"fmt"
	"testing"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2"
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

// NewRegistry starts the local registry.
func NewRegistry(t *testing.T) *Registry {
	t.Helper()
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

// push runs pack (building the manifest in the memory store), tags it and
// copies it into the registry.
func (r *Registry) push(t *testing.T, repo, tag string, pack func(store *memory.Store) (ocispec.Descriptor, error)) Artifact {
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
	if _, err := oras.Copy(ctx, store, tag, dst, tag, oras.DefaultCopyOptions); err != nil {
		t.Fatalf("pushing artifact: %v", err)
	}
	return Artifact{Ref: dst.Reference.Registry + "/" + dst.Reference.Repository + ":" + tag, Digest: manifestDesc.Digest.String()}
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
