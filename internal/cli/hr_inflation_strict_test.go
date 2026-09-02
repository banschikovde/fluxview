package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/helm"
)

// strictHR builds a HelmRelease sourced from a HelmRepository for strict-mode
// inflateHelmReleasesShared tests.
func strictHR(name, repoName string) flux.HelmRelease {
	return flux.HelmRelease{
		Metadata: flux.ObjectMeta{Name: name, Namespace: "apps"},
		Spec: flux.HelmReleaseSpec{
			Chart: flux.HelmReleaseChart{
				Spec: flux.HelmReleaseChartSpec{
					Chart: "podinfo",
					SourceRef: struct {
						Kind      string `yaml:"kind"`
						Name      string `yaml:"name"`
						Namespace string `yaml:"namespace,omitempty"`
					}{
						Kind: flux.KindHelmRepository,
						Name: repoName,
					},
				},
			},
		},
	}
}

// TestInflateHelmReleasesShared_StrictFailsOnUnresolvedSource: in strict mode
// (diff), a HelmRelease whose source cannot be resolved must return an error
// instead of being skipped — a silently skipped HR on one diff side only
// surfaces as a false "added"/"removed" diff.
func TestInflateHelmReleasesShared_StrictFailsOnUnresolvedSource(t *testing.T) {
	hr := []flux.HelmRelease{strictHR("podinfo", "missing-repo")}

	inflater, err := helm.NewInflater()
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}

	outputs, err := inflateHelmReleasesShared(context.Background(), inflater, hr, nil, nil, nil, nil, inflateOptions{strict: true})
	if err == nil {
		t.Fatal("expected error in strict mode when source is unresolved")
	}
	if outputs != nil {
		t.Errorf("expected nil outputs on strict failure, got %d", len(outputs))
	}
	for _, want := range []string{"apps/podinfo", "could not resolve source", "diff would be incomplete"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

// TestInflateHelmReleasesShared_StrictQuietStillFails: the diff comparison
// side runs with quiet=true, which suppresses stderr diagnostics — the error
// must still be returned, otherwise the failure is invisible and the diff
// proceeds over a partial state.
func TestInflateHelmReleasesShared_StrictQuietStillFails(t *testing.T) {
	hr := []flux.HelmRelease{strictHR("podinfo", "missing-repo")}

	inflater, err := helm.NewInflater()
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}

	var inflateErr error
	stderr := captureStderr(func() {
		_, inflateErr = inflateHelmReleasesShared(context.Background(), inflater, hr, nil, nil, nil, nil, inflateOptions{strict: true, quiet: true})
	})
	if inflateErr == nil {
		t.Fatal("expected error even in quiet mode (diff comparison side)")
	}
	if stderr != "" {
		t.Errorf("expected silent stderr in quiet mode, got:\n%s", stderr)
	}
}

// TestInflateHelmReleasesShared_StrictFailsOnChartDownloadError: a chart that
// cannot be downloaded (e.g. no network access) must fail the strict-mode
// build rather than silently drop the HelmRelease's resources from the diff.
// Port 1 on localhost refuses connections immediately without external
// network access.
func TestInflateHelmReleasesShared_StrictFailsOnChartDownloadError(t *testing.T) {
	hr := []flux.HelmRelease{strictHR("podinfo", "charts")}
	repos := []flux.HelmRepository{{
		Metadata: flux.ObjectMeta{Name: "charts", Namespace: "apps"},
		Spec:     flux.HelmRepositorySpec{URL: "http://127.0.0.1:1"},
	}}

	inflater, err := helm.NewInflater()
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}

	_, err = inflateHelmReleasesShared(context.Background(), inflater, hr, repos, nil, nil, nil, inflateOptions{strict: true, quiet: true})
	if err == nil {
		t.Fatal("expected error in strict mode when chart cannot be downloaded")
	}
	for _, want := range []string{"apps/podinfo", "failed to inflate"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

// TestInflateHelmReleasesShared_StrictAggregatesFailures: all failures are
// collected so the user sees every broken HelmRelease in one run.
func TestInflateHelmReleasesShared_StrictAggregatesFailures(t *testing.T) {
	hr := []flux.HelmRelease{strictHR("one", "missing-a"), strictHR("two", "missing-b")}

	inflater, err := helm.NewInflater()
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}

	_, err = inflateHelmReleasesShared(context.Background(), inflater, hr, nil, nil, nil, nil, inflateOptions{strict: true})
	if err == nil {
		t.Fatal("expected aggregated error for two unresolved HelmReleases")
	}
	for _, want := range []string{"2 HelmReleases", "apps/one", "apps/two"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}

// TestInflateHelmReleasesShared_StrictToleratesSuspended: a suspended
// HelmRelease is a deterministic skip (Flux does not reconcile it), identical
// on both diff sides — it must not fail strict mode.
func TestInflateHelmReleasesShared_StrictToleratesSuspended(t *testing.T) {
	h := strictHR("paused", "missing-repo")
	h.Spec.Suspend = true
	hr := []flux.HelmRelease{h}

	inflater, err := helm.NewInflater()
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}

	var outputs [][]byte
	stderr := captureStderr(func() {
		outputs, err = inflateHelmReleasesShared(context.Background(), inflater, hr, nil, nil, nil, nil, inflateOptions{strict: true})
	})
	if err != nil {
		t.Fatalf("suspended HelmRelease must not fail strict mode: %v", err)
	}
	if len(outputs) != 0 {
		t.Errorf("expected 0 outputs for suspended HelmRelease, got %d", len(outputs))
	}
	if !strings.Contains(stderr, "Skipping suspended") {
		t.Errorf("expected suspended-skip warning in non-quiet strict mode, got:\n%s", stderr)
	}
}

// TestInflateHelmReleasesShared_StrictToleratesBucket: Bucket-sourced charts
// are a documented limitation, skipped deterministically on both diff sides —
// warning only, no strict failure.
func TestInflateHelmReleasesShared_StrictToleratesBucket(t *testing.T) {
	h := strictHR("bucket-hr", "my-bucket")
	h.Spec.Chart.Spec.SourceRef.Kind = flux.KindBucket
	hr := []flux.HelmRelease{h}

	inflater, err := helm.NewInflater()
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}

	var outputs [][]byte
	stderr := captureStderr(func() {
		outputs, err = inflateHelmReleasesShared(context.Background(), inflater, hr, nil, nil, nil, nil, inflateOptions{strict: true, repoRoot: t.TempDir()})
	})
	if err != nil {
		t.Fatalf("Bucket-sourced HelmRelease must not fail strict mode: %v", err)
	}
	if len(outputs) != 0 {
		t.Errorf("expected 0 outputs for Bucket-sourced chart, got %d", len(outputs))
	}
	if !strings.Contains(stderr, "not supported") {
		t.Errorf("expected Bucket warning in non-quiet strict mode, got:\n%s", stderr)
	}
}

// TestInflateHelmReleasesShared_StrictFailsOnMissingLocalChart: a
// GitRepository-sourced chart whose path is absent from the checkout makes the
// state unbuildable — strict mode must fail instead of emitting a partial diff.
func TestInflateHelmReleasesShared_StrictFailsOnMissingLocalChart(t *testing.T) {
	h := strictHR("git-hr", "flux-system")
	h.Spec.Chart.Spec.Chart = "./charts/absent"
	h.Spec.Chart.Spec.SourceRef.Kind = flux.KindGitRepository
	hr := []flux.HelmRelease{h}

	inflater, err := helm.NewInflater()
	if err != nil {
		t.Fatalf("NewInflater: %v", err)
	}

	_, err = inflateHelmReleasesShared(context.Background(), inflater, hr, nil, nil, nil, nil, inflateOptions{strict: true, repoRoot: t.TempDir()})
	if err == nil {
		t.Fatal("expected error in strict mode for missing local chart path")
	}
	for _, want := range []string{"apps/git-hr", "not found locally"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err.Error(), want)
		}
	}
}
