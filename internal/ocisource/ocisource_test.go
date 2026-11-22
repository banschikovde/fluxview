package ocisource

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/ocisource/ocisourcetest"
)

// dockerConfigFor points DOCKER_CONFIG at a config holding the test
// registry's credentials — the pull-side analog of `docker login`.
func dockerConfigFor(t *testing.T, reg *ocisourcetest.Registry) {
	t.Helper()
	dir := t.TempDir()
	enc := base64.StdEncoding.EncodeToString([]byte(reg.Username + ":" + reg.Password))
	config := `{"auths": {"` + reg.URL + `": {"auth": "` + enc + `"}}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0o600); err != nil {
		t.Fatalf("writing docker config: %v", err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
}

// noDockerConfigFor points DOCKER_CONFIG at an empty directory — anonymous
// pulls only.
func noDockerConfigFor(t *testing.T) {
	t.Setenv("DOCKER_CONFIG", t.TempDir())
}

// ociRepo builds a flux.OCIRepository for the test registry.
func ociRepo(url string, ref *flux.OCIRepositoryRef) flux.OCIRepository {
	return flux.OCIRepository{
		Metadata: flux.ObjectMeta{Name: "app", Namespace: "flux-system"},
		Spec:     flux.OCIRepositorySpec{URL: url, Ref: ref},
	}
}

// readArtifactFile reads one file out of a fetched artifact directory.
func readArtifactFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading %s from artifact: %v", name, err)
	}
	return string(data)
}

// failingTransport fails every request — proves a code path touches no
// network.
type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("network disabled for this test")
}

// offlineFetcher returns a Fetcher over cacheDir whose every network
// request fails.
func offlineFetcher(cacheDir string, ttl time.Duration) *Fetcher {
	f := NewFetcher(cacheDir, ttl)
	f.clientOnce.Do(func() {
		f.client = &authClientBundle{client: &auth.Client{
			Client: &http.Client{Transport: failingTransport{}},
		}}
	})
	return f
}

func TestEnsure_Tag(t *testing.T) {
	reg := ocisourcetest.NewRegistry(t)
	dockerConfigFor(t, reg)

	art := reg.PushArtifact(t, "u/app", "v1", ocisourcetest.DefaultLayerMediaType, map[string]string{
		"apps/base/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n",
		"apps/base/cm.yaml":            "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: from-artifact\n",
	})

	f := NewFetcher(t.TempDir(), time.Hour)
	defer f.Close()

	dir, err := f.Ensure(context.Background(), ociRepo("oci://"+reg.URL+"/u/app", &flux.OCIRepositoryRef{Tag: "v1"}))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := readArtifactFile(t, dir, "apps/base/cm.yaml"); !strings.Contains(got, "from-artifact") {
		t.Errorf("artifact content mismatch, got:\n%s", got)
	}
	if !strings.HasPrefix(art.Digest, "sha256:") {
		t.Errorf("pushed digest malformed: %s", art.Digest)
	}
}

func TestEnsure_Digest(t *testing.T) {
	reg := ocisourcetest.NewRegistry(t)
	dockerConfigFor(t, reg)

	art := reg.PushArtifact(t, "u/app", "v1", ocisourcetest.DefaultLayerMediaType, map[string]string{
		"cm.yaml": "kind: ConfigMap\n",
	})
	f := NewFetcher(t.TempDir(), time.Hour)
	defer f.Close()

	dir, err := f.Ensure(context.Background(), ociRepo("oci://"+reg.URL+"/u/app", &flux.OCIRepositoryRef{Digest: art.Digest}))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := readArtifactFile(t, dir, "cm.yaml"); !strings.Contains(got, "ConfigMap") {
		t.Errorf("artifact content mismatch, got:\n%s", got)
	}
}

func TestEnsure_SemverPicksHighestMatch(t *testing.T) {
	reg := ocisourcetest.NewRegistry(t)
	dockerConfigFor(t, reg)

	reg.PushArtifact(t, "u/app", "v1.0.0", ocisourcetest.DefaultLayerMediaType, map[string]string{"cm.yaml": "version: 1.0.0\n"})
	reg.PushArtifact(t, "u/app", "v1.2.0", ocisourcetest.DefaultLayerMediaType, map[string]string{"cm.yaml": "version: 1.2.0\n"})
	reg.PushArtifact(t, "u/app", "v2.0.0", ocisourcetest.DefaultLayerMediaType, map[string]string{"cm.yaml": "version: 2.0.0\n"})

	f := NewFetcher(t.TempDir(), time.Hour)
	defer f.Close()

	dir, err := f.Ensure(context.Background(), ociRepo("oci://"+reg.URL+"/u/app", &flux.OCIRepositoryRef{Semver: ">=1.0.0 <2.0.0"}))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := readArtifactFile(t, dir, "cm.yaml"); !strings.Contains(got, "version: 1.2.0") {
		t.Errorf("semver must pick the highest tag inside the range, got:\n%s", got)
	}
}

func TestEnsure_NoRefUsesLiteralLatestTag(t *testing.T) {
	reg := ocisourcetest.NewRegistry(t)
	dockerConfigFor(t, reg)

	reg.PushArtifact(t, "u/app", "v1.0.0", ocisourcetest.DefaultLayerMediaType, map[string]string{"cm.yaml": "version: 1.0.0\n"})
	reg.PushArtifact(t, "u/app", "latest", ocisourcetest.DefaultLayerMediaType, map[string]string{"cm.yaml": "version: latest\n"})

	f := NewFetcher(t.TempDir(), time.Hour)
	defer f.Close()

	dir, err := f.Ensure(context.Background(), ociRepo("oci://"+reg.URL+"/u/app", nil))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if got := readArtifactFile(t, dir, "cm.yaml"); !strings.Contains(got, "version: latest") {
		t.Errorf("an empty ref must resolve the literal latest tag, got:\n%s", got)
	}
}

func TestEnsure_LayerSelector(t *testing.T) {
	reg := ocisourcetest.NewRegistry(t)
	dockerConfigFor(t, reg)

	const chartLayer = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	reg.PushLayeredArtifact(t, "u/app", "v1",
		ocisourcetest.LayerSpec{MediaType: "application/vnd.acme.rocket.layer.v1+tar", Files: map[string]string{"wrong.txt": "not selected\n"}},
		ocisourcetest.LayerSpec{MediaType: chartLayer, Files: map[string]string{"gateway-helm/charts/crds/crds.yaml": "kind: CustomResourceDefinition\n"}},
	)

	f := NewFetcher(t.TempDir(), time.Hour)
	defer f.Close()
	base := "oci://" + reg.URL + "/u/app"

	// No selector: the first layer.
	dir, err := f.Ensure(context.Background(), ociRepo(base, &flux.OCIRepositoryRef{Tag: "v1"}))
	if err != nil {
		t.Fatalf("Ensure without selector: %v", err)
	}
	if got := readArtifactFile(t, dir, "wrong.txt"); !strings.Contains(got, "not selected") {
		t.Errorf("no selector must take the first layer, got:\n%s", got)
	}

	// mediaType selector: the first matching layer, regardless of
	// operation (copy and extract select identically for kustomize).
	repo := ociRepo(base, &flux.OCIRepositoryRef{Tag: "v1"})
	repo.Spec.LayerSelector = &flux.OCILayerSelector{MediaType: chartLayer, Operation: "copy"}
	dir, err = f.Ensure(context.Background(), repo)
	if err != nil {
		t.Fatalf("Ensure with selector: %v", err)
	}
	if got := readArtifactFile(t, dir, "gateway-helm/charts/crds/crds.yaml"); !strings.Contains(got, "CustomResourceDefinition") {
		t.Errorf("selector must pick the matching layer (both extract and copy untar for kustomize), got:\n%s", got)
	}

	// No layer matches: a clear error.
	repo.Spec.LayerSelector = &flux.OCILayerSelector{MediaType: "application/vnd.no.such.layer"}
	if _, err := f.Ensure(context.Background(), repo); err == nil || !strings.Contains(err.Error(), "no layer with media type") {
		t.Errorf("expected a no-matching-layer error, got: %v", err)
	}
}

func TestEnsure_Auth_DockerConfig(t *testing.T) {
	reg := ocisourcetest.NewRegistry(t)
	dockerConfigFor(t, reg)
	reg.PushArtifact(t, "u/private", "v1", ocisourcetest.DefaultLayerMediaType, map[string]string{"cm.yaml": "kind: ConfigMap\n"})

	f := NewFetcher(t.TempDir(), time.Hour)
	defer f.Close()
	if _, err := f.Ensure(context.Background(), ociRepo("oci://"+reg.URL+"/u/private", &flux.OCIRepositoryRef{Tag: "v1"})); err != nil {
		t.Fatalf("credentials from the docker config must authenticate the pull: %v", err)
	}
}

func TestEnsure_Auth_UnauthorizedHintsDockerConfig(t *testing.T) {
	reg := ocisourcetest.NewRegistry(t)
	noDockerConfigFor(t)
	reg.PushArtifact(t, "u/private", "v1", ocisourcetest.DefaultLayerMediaType, map[string]string{"cm.yaml": "kind: ConfigMap\n"})

	// 127.0.0.1, not localhost: the credential store's native fallback
	// (macOS keychain) may hold a "localhost" entry written by other
	// tools' test logins — the anonymous expectations below must not
	// depend on the machine's keychain.
	url := strings.Replace(reg.URL, "localhost", "127.0.0.1", 1)
	f := NewFetcher(t.TempDir(), time.Hour)
	defer f.Close()
	_, err := f.Ensure(context.Background(), ociRepo("oci://"+url+"/u/private", &flux.OCIRepositoryRef{Tag: "v1"}))
	if err == nil {
		t.Fatal("anonymous pull against an htpasswd registry must fail")
	}
	// The challenge fails client-side (no Basic credential) or as a typed
	// 401 — both must carry the docker-config remedy.
	for _, want := range []string{"requires credentials", "Docker config", "spec.secretRef"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q, got: %v", want, err)
		}
	}
}

func TestEnsure_WarmCacheWorksOffline(t *testing.T) {
	reg := ocisourcetest.NewRegistry(t)
	dockerConfigFor(t, reg)
	art := reg.PushArtifact(t, "u/app", "v1", ocisourcetest.DefaultLayerMediaType, map[string]string{"cm.yaml": "kind: ConfigMap\n"})

	cacheDir := t.TempDir()
	warm := NewFetcher(cacheDir, time.Hour)
	defer warm.Close()
	if _, err := warm.Ensure(context.Background(), ociRepo("oci://"+reg.URL+"/u/app", &flux.OCIRepositoryRef{Digest: art.Digest})); err != nil {
		t.Fatalf("warm-up fetch: %v", err)
	}

	// A digest-pinned artifact is complete in the cache: a later run with
	// the network down must succeed without a single request.
	cold := offlineFetcher(cacheDir, time.Hour)
	defer cold.Close()
	dir, err := cold.Ensure(context.Background(), ociRepo("oci://"+reg.URL+"/u/app", &flux.OCIRepositoryRef{Digest: art.Digest}))
	if err != nil {
		t.Fatalf("digest-pinned artifact must be served from the warm cache offline: %v", err)
	}
	if got := readArtifactFile(t, dir, "cm.yaml"); !strings.Contains(got, "ConfigMap") {
		t.Errorf("cached artifact content mismatch, got:\n%s", got)
	}

	// A floating tag within the TTL resolves from the pointer, also offline.
	tagged := offlineFetcher(cacheDir, time.Hour)
	defer tagged.Close()
	warmTag := NewFetcher(cacheDir, time.Hour)
	defer warmTag.Close()
	if _, err := warmTag.Ensure(context.Background(), ociRepo("oci://"+reg.URL+"/u/app", &flux.OCIRepositoryRef{Tag: "v1"})); err != nil {
		t.Fatalf("tag fetch: %v", err)
	}
	if _, err := tagged.Ensure(context.Background(), ociRepo("oci://"+reg.URL+"/u/app", &flux.OCIRepositoryRef{Tag: "v1"})); err != nil {
		t.Fatalf("a fresh tag pointer must resolve offline within the TTL: %v", err)
	}
}

func TestEnsure_TTLZeroReResolves(t *testing.T) {
	reg := ocisourcetest.NewRegistry(t)
	dockerConfigFor(t, reg)
	reg.PushArtifact(t, "u/app", "v1", ocisourcetest.DefaultLayerMediaType, map[string]string{"cm.yaml": "kind: ConfigMap\n"})

	cacheDir := t.TempDir()
	warm := NewFetcher(cacheDir, time.Hour)
	defer warm.Close()
	if _, err := warm.Ensure(context.Background(), ociRepo("oci://"+reg.URL+"/u/app", &flux.OCIRepositoryRef{Tag: "v1"})); err != nil {
		t.Fatalf("warm-up fetch: %v", err)
	}

	// ttl=0 ignores the pointer: the offline run must fail at resolution.
	cold := NewFetcher(cacheDir, 0)
	cold.clientOnce.Do(func() {
		cold.client = &authClientBundle{client: &auth.Client{Client: &http.Client{Transport: failingTransport{}}}}
	})
	defer cold.Close()
	if _, err := cold.Ensure(context.Background(), ociRepo("oci://"+reg.URL+"/u/app", &flux.OCIRepositoryRef{Tag: "v1"})); err == nil {
		t.Fatal("ttl=0 must re-resolve the tag over the network, which is down in this test")
	}
}

func TestEnsure_CacheDisabledLeavesNothingBehind(t *testing.T) {
	reg := ocisourcetest.NewRegistry(t)
	dockerConfigFor(t, reg)
	reg.PushArtifact(t, "u/app", "v1", ocisourcetest.DefaultLayerMediaType, map[string]string{"cm.yaml": "kind: ConfigMap\n"})

	f := NewFetcher("disabled", time.Hour)
	dir, err := f.Ensure(context.Background(), ociRepo("oci://"+reg.URL+"/u/app", &flux.OCIRepositoryRef{Tag: "v1"}))
	if err != nil {
		t.Fatalf("Ensure with a disabled cache: %v", err)
	}
	if !strings.HasPrefix(dir, os.TempDir()) {
		t.Errorf("a disabled cache must fetch into the system temp dir, got %s", dir)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("Close must remove the cache-disabled fetch at %s", dir)
	}
}

func TestEnsure_RejectsBadSpecs(t *testing.T) {
	reg := ocisourcetest.NewRegistry(t)
	dockerConfigFor(t, reg)
	f := NewFetcher(t.TempDir(), time.Hour)
	defer f.Close()

	for _, tc := range []struct {
		name string
		repo flux.OCIRepository
		want string
	}{
		{
			name: "no url",
			repo: flux.OCIRepository{Metadata: flux.ObjectMeta{Name: "a", Namespace: "ns"}},
			want: "has no spec.url",
		},
		{
			name: "no oci scheme",
			repo: ociRepo("https://example.com/repo", nil),
			want: "must start with oci://",
		},
		{
			name: "tag in url",
			repo: ociRepo("oci://"+reg.URL+"/u/app:v1", nil),
			want: "must not contain a tag",
		},
		{
			name: "digest in url",
			repo: ociRepo("oci://"+reg.URL+"/u/app@sha256:abc", nil),
			want: "must not contain a digest",
		},
		{
			name: "unsupported provider",
			repo: func() flux.OCIRepository {
				r := ociRepo("oci://"+reg.URL+"/u/app", nil)
				r.Spec.Provider = "aws"
				return r
			}(),
			want: `provider "aws" is not supported`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.Ensure(context.Background(), tc.repo)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected error containing %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestEnsure_TraversalLayerFailsClosed(t *testing.T) {
	reg := ocisourcetest.NewRegistry(t)
	dockerConfigFor(t, reg)

	// An archive that writes outside the extraction root.
	evil := buildTarGz(t, map[string]string{
		"ok.txt":      "fine\n",
		"../evil.txt": "escaped\n",
	})
	reg.PushLayerBytes(t, "u/evil", "v1", ocisourcetest.DefaultLayerMediaType, evil)

	f := NewFetcher(t.TempDir(), time.Hour)
	defer f.Close()
	_, err := f.Ensure(context.Background(), ociRepo("oci://"+reg.URL+"/u/evil", &flux.OCIRepositoryRef{Tag: "v1"}))
	if err == nil || !strings.Contains(err.Error(), "escapes the extraction root") {
		t.Fatalf("a traversal archive must fail the fetch, got: %v", err)
	}
	// Nothing leaked next to the cache: only failed tmp dirs (removed).
	if _, err := os.Stat(filepath.Join(f.dir, "..", "evil.txt")); err == nil {
		t.Fatal("../evil.txt must not exist")
	}
}

func TestEnsure_AbsolutePathLayerFailsClosed(t *testing.T) {
	reg := ocisourcetest.NewRegistry(t)
	dockerConfigFor(t, reg)

	evil := buildTarGz(t, map[string]string{"/abs/evil.txt": "escaped\n"})
	reg.PushLayerBytes(t, "u/evil2", "v1", ocisourcetest.DefaultLayerMediaType, evil)

	f := NewFetcher(t.TempDir(), time.Hour)
	defer f.Close()
	_, err := f.Ensure(context.Background(), ociRepo("oci://"+reg.URL+"/u/evil2", &flux.OCIRepositoryRef{Tag: "v1"}))
	if err == nil || !strings.Contains(err.Error(), "escapes the extraction root") {
		t.Fatalf("an absolute-path archive must fail the fetch, got: %v", err)
	}
	if _, err := os.Stat("/abs/evil.txt"); err == nil {
		t.Fatal("/abs/evil.txt must not exist")
	}
}

func TestPickSemverTag(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tags       []string
		constraint string
		want       string
	}{
		{name: "highest in range", tags: []string{"v1.0.0", "v1.2.0", "v2.0.0"}, constraint: ">=1.0.0 <2.0.0", want: "v1.2.0"},
		{name: "v prefix tolerated", tags: []string{"1.1.0", "v1.2.0"}, constraint: "*", want: "v1.2.0"},
		{name: "underscore plus", tags: []string{"1.2.3-beta.1_4"}, constraint: ">=1.0.0-0", want: "1.2.3-beta.1_4"},
		{name: "non-semver ignored", tags: []string{"latest", "1.0.0"}, constraint: "*", want: "1.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pickSemverTag(tc.tags, tc.constraint)
			if err != nil {
				t.Fatalf("pickSemverTag: %v", err)
			}
			if got != tc.want {
				t.Errorf("pickSemverTag(%v, %q) = %q, want %q", tc.tags, tc.constraint, got, tc.want)
			}
		})
	}
}

func TestPickSemverTag_NoMatchNamesConstraint(t *testing.T) {
	_, err := pickSemverTag([]string{"1.0.0"}, ">=2.0.0")
	if err == nil {
		t.Fatal("expected a no-match error")
	}
	if !strings.Contains(err.Error(), ">=2.0.0") {
		t.Errorf("the error must name the constraint, got: %v", err)
	}
	if !strings.Contains(err.Error(), "1.0.0") {
		t.Errorf("the error must list the available tags, got: %v", err)
	}
}

// buildTarGz packs files (name → content) without any name validation —
// the malicious-archive counterpart of ocisourcetest.tarGz.
func buildTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}); err != nil {
			t.Fatalf("writing header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	return buf.Bytes()
}

func TestPickSemverTag_ErrorCapsTagList(t *testing.T) {
	tags := make([]string, 30)
	for i := range tags {
		tags[i] = fmt.Sprintf("1.0.%d", i)
	}
	_, err := pickSemverTag(tags, ">=99.0.0")
	if err == nil {
		t.Fatal("expected no-match error")
	}
	if !strings.Contains(err.Error(), "… and 10 more") {
		t.Errorf("the tag list must cap at 20 with a remainder, got: %v", err)
	}
	if strings.Contains(err.Error(), "1.0.29,") {
		t.Errorf("tags past the cap must not be listed, got: %v", err)
	}
}

func TestFetchSynced_TimeoutRenewsPerAttempt(t *testing.T) {
	// A registry that accepts the connection and never answers: every
	// fetch attempt dies on its own timeout. The timeout scope is created
	// inside the attempt, so a retry after a failed winner starts with the
	// full budget — two attempts must both fail on the deadline, not the
	// second on the first one's residue.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	f := NewFetcher(t.TempDir(), time.Hour, WithTimeout(150*time.Millisecond))
	defer f.Close()
	repo := ociRepo("oci://"+host+"/u/hang", &flux.OCIRepositoryRef{Digest: "sha256:" + strings.Repeat("ab", 32)})

	for attempt := 1; attempt <= 2; attempt++ {
		start := time.Now()
		base := host + "/u/hang"
		err := f.fetchSynced(context.Background(), base, base, "digest:sha256:"+strings.Repeat("ab", 32), repo, filepath.Join(f.dir, "data", fmt.Sprintf("attempt-%d", attempt)))
		elapsed := time.Since(start)
		if err == nil {
			t.Fatalf("attempt %d: a hung registry must fail", attempt)
		}
		if elapsed < 100*time.Millisecond {
			t.Fatalf("attempt %d: failed too fast (%s) — the timeout did not govern it", attempt, elapsed)
		}
		if elapsed > 3*time.Second {
			t.Fatalf("attempt %d: the timeout must fire promptly, took %s", attempt, elapsed)
		}
	}
}
