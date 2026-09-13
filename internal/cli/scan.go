package cli

import (
	"context"

	"github.com/banschikovde/fluxview/internal/flux"
)

// scanCache memoizes the per-root scans shared across one pipeline run:
// snapshot-cached Parsers (one resource walk per root instead of one walk per
// ParseXxx call — the HR pipeline alone used to walk clusterPath 7+ times)
// and DiscoverKustomizeDirsAndFiles results (one kustomize-directory
// discovery walk per root instead of one per resolver and overlay builder).
//
// One instance must be created per command invocation and threaded through
// the pipeline. Not safe for concurrent use — CLI pipelines are sequential.
type scanCache struct {
	parsers map[string]*flux.Parser
	dirs    map[string]kustDirsResult
}

// kustDirsResult caches one DiscoverKustomizeDirsAndFiles call, including
// its error: a discovery that failed is not retried within the same command.
type kustDirsResult struct {
	buildDirs []string
	fileDirs  []string
	err       error
}

func newScanCache() *scanCache {
	return &scanCache{
		parsers: make(map[string]*flux.Parser),
		dirs:    make(map[string]kustDirsResult),
	}
}

// parserFor returns a Parser rooted at root, creating it on first use. All
// ParseXxx calls on the returned Parser share one cached resource snapshot,
// so the tree under root is walked (and each YAML file parsed) once.
func (c *scanCache) parserFor(root string) *flux.Parser {
	if p, ok := c.parsers[root]; ok {
		return p
	}
	p := flux.NewParser(root)
	c.parsers[root] = p
	return p
}

// kustDirsAndFiles returns the memoized DiscoverKustomizeDirsAndFiles result
// for root. Shared by resolveConfigMaps/resolveSecrets (kustomize builds for
// substitution sources), buildKustomizeOverlays and
// buildSubdirectoriesAndLooseFiles — previously each performed its own walk.
func (c *scanCache) kustDirsAndFiles(ctx context.Context, root string) (buildDirs, fileDirs []string, err error) {
	if r, ok := c.dirs[root]; ok {
		return r.buildDirs, r.fileDirs, r.err
	}
	buildDirs, fileDirs, err = flux.DiscoverKustomizeDirsAndFiles(ctx, root)
	c.dirs[root] = kustDirsResult{buildDirs: buildDirs, fileDirs: fileDirs, err: err}
	return buildDirs, fileDirs, err
}
