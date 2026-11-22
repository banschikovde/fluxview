package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/ocisource"
)

// EnvLocalOCISources lists (CSV, "namespace/name") the OCIRepositories whose
// artifacts live in this very repository — pushed by `flux push artifact`
// in CI — so their Kustomizations resolve paths locally instead of
// fetching the registry (the pre-1.9 behavior, source-last).
const EnvLocalOCISources = "FLUXVIEW_OCI_LOCAL_SOURCES"

// ociSourceEnv carries the OCI artifact fetcher of the Kustomization
// pipeline and the local-artifact allowlist. A nil ociSourceEnv disables
// fetching — OCIRepository-sourced Kustomizations resolve locally.
type ociSourceEnv struct {
	// fetcher downloads artifacts; nil when noFetch.
	fetcher *ocisource.Fetcher
	// noFetch disables all registry requests (--no-oci-source-fetch).
	noFetch bool
	// local is the FLUXVIEW_OCI_LOCAL_SOURCES allowlist ("ns/name"): these
	// OCIRepositories are this repository's own artifacts and always
	// resolve locally.
	local map[string]bool
}

// newOCISourceEnv builds the env: fetcher over the OCI source cache unless
// fetching is disabled, plus the local-artifact allowlist.
func newOCISourceEnv(ksCache kustomizeCacheOptions, noFetch bool) *ociSourceEnv {
	e := &ociSourceEnv{
		noFetch: noFetch,
		local:   parseLocalOCISources(os.Getenv(EnvLocalOCISources)),
	}
	if !noFetch {
		e.fetcher = ocisource.NewFetcher(ksCache.ociSourceDir, ksCache.ociSourceTtl,
			ocisource.WithTimeout(ksCache.ociSourceTimeout))
	}
	return e
}

// parseLocalOCISources parses the CSV allowlist into a set.
func parseLocalOCISources(v string) map[string]bool {
	set := make(map[string]bool)
	for _, entry := range strings.Split(v, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			set[entry] = true
		}
	}
	return set
}

// Close releases the env's resources (temporary fetches made while the OCI
// source cache was disabled; cached artifacts are kept). Safe on a nil
// env; call from a defer at the command level.
func (e *ociSourceEnv) Close() error {
	if e == nil {
		return nil
	}
	return e.fetcher.Close()
}

// lookup returns the OCIRepository a Kustomization's sourceRef points at.
// The sourceRef namespace defaults to the Kustomization's own namespace,
// as in Flux. Unlike git there is no "external vs local" identity check —
// any found OCIRepository is a registry artifact; the local-allowlist
// decision is isLocal's.
func (e *ociSourceEnv) lookup(repos map[string]flux.OCIRepository, ks flux.Kustomization) (flux.OCIRepository, bool) {
	if e == nil {
		return flux.OCIRepository{}, false
	}
	if ks.Spec.SourceRef.Kind != flux.KindOCIRepository || ks.Spec.SourceRef.Name == "" {
		return flux.OCIRepository{}, false
	}
	ns := ks.Spec.SourceRef.Namespace
	if ns == "" {
		ns = ks.Metadata.Namespace
	}
	repo, ok := repos[ns+"/"+ks.Spec.SourceRef.Name]
	return repo, ok
}

// isLocal reports whether the Kustomization's OCIRepository is listed in
// FLUXVIEW_OCI_LOCAL_SOURCES: its artifact is this repository's own
// content (flux push artifact of this repo), so spec.path resolves
// locally and no fetch happens.
func (e *ociSourceEnv) isLocal(ks flux.Kustomization) bool {
	if e == nil {
		return false
	}
	ns := ks.Spec.SourceRef.Namespace
	if ns == "" {
		ns = ks.Metadata.Namespace
	}
	return e.local[ns+"/"+ks.Spec.SourceRef.Name]
}

// fetchEnabled reports whether this env fetches artifacts at all: with
// the kill switch on, Kustomizations resolve locally — the pre-fetch
// behavior, byte-for-byte (no registry identity, no fetch attempt).
func (e *ociSourceEnv) fetchEnabled() bool {
	return e != nil && !e.noFetch && e.fetcher != nil
}

// ensure fetches (or reuses) the extracted artifact of an OCIRepository.
// With fetching disabled it fails before any network: the caller treats it
// exactly like an unreachable registry. The message reads through the
// wrapper ("fetching <source> for <ks> failed: <message>") — keep it a
// bare cause, not a sentence of its own.
func (e *ociSourceEnv) ensure(ctx context.Context, repo flux.OCIRepository) (string, error) {
	if e.noFetch {
		return "", fmt.Errorf("disabled by --no-oci-source-fetch or %s", ocisource.EnvNoFetch)
	}
	return e.fetcher.Ensure(ctx, repo)
}

// sourceEnvs bundles the external source environments of one command run:
// external git clones and OCI artifacts. Both sides of a diff share one
// bundle, so both reuse the same caches.
type sourceEnvs struct {
	git *gitSourceEnv
	oci *ociSourceEnv
}

// newSourceEnvs builds both envs for a command run over the real
// repository root.
func newSourceEnvs(ctx context.Context, realRepoRoot string, ksCache kustomizeCacheOptions, noGitFetch, noOCIFetch bool) *sourceEnvs {
	return &sourceEnvs{
		git: newGitSourceEnv(ctx, realRepoRoot, ksCache, noGitFetch),
		oci: newOCISourceEnv(ksCache, noOCIFetch),
	}
}

// Close releases both envs. Safe on a nil bundle.
func (s *sourceEnvs) Close() error {
	if s == nil {
		return nil
	}
	return errors.Join(s.git.Close(), s.oci.Close())
}

// registerNoOCISourceFetchFlag registers the OCI artifact network kill
// switch shared by the commands. Lives here, next to the ociSourceEnv it
// toggles. Default from the environment, an explicit flag wins for the
// run.
func registerNoOCISourceFetchFlag(cmd *cobra.Command, noFetch *bool) {
	cmd.Flags().BoolVar(noFetch, "no-oci-source-fetch", ocisource.DefaultNoFetch(),
		"Do not fetch OCIRepository artifacts (no network at all): Kustomizations pointing at them resolve paths locally — for slow or offline networks (env: "+ocisource.EnvNoFetch+")")
}
