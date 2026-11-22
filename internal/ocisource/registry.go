package ocisource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/credentials"
	"oras.land/oras-go/v2/registry/remote/errcode"

	"github.com/banschikovde/fluxview/internal/flux"
)

// digestPrefix is the canonical resolved-ref spelling: an artifact is
// always resolved to "digest:<manifest digest>" before it is cached.
const digestPrefix = "digest:"

// userAgent identifies fluxview to registries and their token services.
const userAgent = "fluxview"

// authClientBundle bundles the lazily-built registry client: the token
// cache and the docker-config credential store live for the Fetcher
// lifetime so repeated fetches reuse both.
type authClientBundle struct {
	client *auth.Client
}

// authClient returns the shared registry client, building it on first use:
// an oras auth.Client over a dedicated transport (connection-establishing
// phases capped — a hung registry must not stall a fetch beyond them; the
// overall per-artifact limit is the Fetcher timeout), with the token cache
// enabled and credentials read from the Docker config ($DOCKER_CONFIG or
// ~/.docker/config.json, including credential helpers). An unreadable
// Docker config degrades to anonymous access — a broken config file must
// not break public artifact pulls; private ones surface a 401 with the
// docker-config hint. spec.secretRef Secrets are never read: they live in
// the cluster, not on this machine.
func (f *Fetcher) authClient() *auth.Client {
	f.clientOnce.Do(func() {
		credFunc := auth.CredentialFunc(func(context.Context, string) (auth.Credential, error) {
			return auth.EmptyCredential, nil
		})
		if store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{
			DetectDefaultNativeStore: true,
		}); err == nil {
			credFunc = credentials.Credential(store)
		}
		c := &auth.Client{
			Client: &http.Client{
				Transport: &http.Transport{
					Proxy:                 http.ProxyFromEnvironment,
					TLSHandshakeTimeout:   30 * time.Second,
					ResponseHeaderTimeout: 30 * time.Second,
				},
			},
			Credential: credFunc,
			Cache:      auth.NewCache(),
		}
		c.Header = http.Header{"User-Agent": []string{userAgent}}
		f.client = &authClientBundle{client: c}
	})
	if f.client == nil {
		return nil
	}
	return f.client.client
}

// repository builds the oras remote repository handle for base. Plain
// HTTP is used for spec.insecure registries and loopback hosts (test and
// local registries) — the same inference as the Helm OCI charts path.
func (f *Fetcher) repository(base string, insecure bool) (*remote.Repository, error) {
	repo, err := remote.NewRepository(base)
	if err != nil {
		return nil, fmt.Errorf("invalid repository reference %q: %w", base, err)
	}
	repo.PlainHTTP = insecure || loopbackHost(base)
	repo.Client = f.authClient()
	return repo, nil
}

// loopbackHost reports whether the registry host of base is a loopback
// address, where plain HTTP is the norm.
func loopbackHost(base string) bool {
	host := strings.SplitN(base, "/", 2)[0]
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "localhost" || host == "::1" || strings.HasPrefix(host, "127.")
}

// listTags returns all tags of the repository.
func listTags(ctx context.Context, repo *remote.Repository) ([]string, error) {
	var tags []string
	err := repo.Tags(ctx, "", func(page []string) error {
		tags = append(tags, page...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tags, nil
}

// fetchManifest downloads the artifact manifest at digest and verifies its
// content against the digest before decoding it.
func fetchManifest(ctx context.Context, repo *remote.Repository, digest string) (ocispec.Manifest, error) {
	desc, rc, err := repo.FetchReference(ctx, digest)
	if err != nil {
		return ocispec.Manifest{}, err
	}
	defer rc.Close()
	data, err := verifyWhileReading(rc, desc)
	if err != nil {
		return ocispec.Manifest{}, fmt.Errorf("manifest digest mismatch: %w", err)
	}
	if desc.MediaType == ocispec.MediaTypeImageIndex {
		return ocispec.Manifest{}, fmt.Errorf("multi-platform index artifacts are not supported")
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return ocispec.Manifest{}, fmt.Errorf("decoding manifest: %w", err)
	}
	return manifest, nil
}

// selectLayer picks the artifact layer per Flux source-controller
// semantics: no layerSelector — the first layer; a mediaType — the first
// layer matching it (operation extract and copy select identically: for
// Kustomization reconciliation the artifact tarball is untarred either
// way, so fluxview always extracts). The layer must be tar+gzip.
func selectLayer(manifest ocispec.Manifest, sel *flux.OCILayerSelector) (ocispec.Descriptor, error) {
	if len(manifest.Layers) == 0 {
		return ocispec.Descriptor{}, fmt.Errorf("artifact has no layers")
	}
	if sel == nil || sel.MediaType == "" {
		return manifest.Layers[0], nil
	}
	for _, l := range manifest.Layers {
		if l.MediaType == sel.MediaType {
			return l, nil
		}
	}
	available := make([]string, 0, len(manifest.Layers))
	for _, l := range manifest.Layers {
		available = append(available, l.MediaType)
	}
	return ocispec.Descriptor{}, fmt.Errorf("no layer with media type %q (available: %s)",
		sel.MediaType, strings.Join(available, ", "))
}

// verifyWhileReading reads r to completion, verifying the bytes against
// desc.Digest.
func verifyWhileReading(r io.Reader, desc ocispec.Descriptor) ([]byte, error) {
	dig, err := digest.Parse(desc.Digest.String())
	if err != nil {
		return nil, fmt.Errorf("parsing digest %q: %w", desc.Digest.String(), err)
	}
	verifier := dig.Verifier()
	data, err := io.ReadAll(io.TeeReader(r, verifier))
	if err != nil {
		return nil, err
	}
	if !verifier.Verified() {
		return nil, fmt.Errorf("content does not match %s", dig)
	}
	return data, nil
}

// explainAuthStatus decorates registry authentication failures with the
// remedy: where fluxview reads credentials from and what happens to
// spec.secretRef (named when the manifest sets one). Other errors pass
// through unchanged.
func explainAuthStatus(err error, secretRef *flux.LocalObjectReference) error {
	if !isAuthFailure(err) {
		return err
	}
	secret := "a spec.secretRef Secret is only available in-cluster"
	if secretRef != nil && secretRef.Name != "" {
		secret = fmt.Sprintf("spec.secretRef %q is ignored outside the cluster; provide the same credentials via the Docker config", secretRef.Name)
	}
	return fmt.Errorf("%v; the registry requires credentials — fluxview reads them from the Docker config ($DOCKER_CONFIG or ~/.docker/config.json); %s", err, secret)
}

// isAuthFailure reports whether err is a registry authentication refusal:
// a typed 401/403 response, or a Basic challenge that failed client-side
// for lack of credentials.
func isAuthFailure(err error) bool {
	var respErr *errcode.ErrorResponse
	if errors.As(err, &respErr) &&
		(respErr.StatusCode == http.StatusUnauthorized || respErr.StatusCode == http.StatusForbidden) {
		return true
	}
	return errors.Is(err, auth.ErrBasicCredentialNotFound)
}
