package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cyphar/filepath-securejoin"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/git"
	"github.com/banschikovde/fluxview/internal/helm"
	"github.com/banschikovde/fluxview/internal/inventory"
)

// InventoryFlags holds flags for the inventory command.
type InventoryFlags struct {
	Path          string
	Namespace     string
	Source        string
	Output        string
	Sort          string
	NoHeaders     bool
	Rules         string
	AllCRs        bool
	SplitUmbrella bool
	BranchOrig    string
	HelmCache     helmCacheOptions
	KsCache       kustomizeCacheOptions
	NoFetch       bool
	SSHHosts      string
	SSHAcceptNew  bool
}

func newInventoryCmd() *cobra.Command {
	flags := &InventoryFlags{}

	cmd := &cobra.Command{
		Use:   "inventory [flags]",
		Short: "List all software deployed by the GitOps repository",
		Long: `List all software deployed in the cluster according to the local
GitOps repository: Helm releases, plain-manifest workloads, operator
custom resources and CRD groups, with versions and images.

Built from the same pipeline as build ks / build hr. Charts render on
every run to resolve the IMAGES column (a warm chart cache keeps it
cheap; a missing image reads as "none"). Output is sectioned per source
(Helm releases, Operator custom resources, Plain manifests, CRD groups),
as a fixed-width table or GitHub-flavored Markdown. Works without a
cluster connection.

Examples:
  fluxview inventory --path clusters/prod/flux/
  fluxview inventory helm --path clusters/prod/flux/
  fluxview inventory --path clusters/prod/flux/ -n monitoring --source helm,cr -o markdown
  fluxview inventory --path clusters/prod/flux/ --branch-orig master -o markdown`,
		RunE: func(cmd *cobra.Command, args []string) error {
			applyGitSourceAuthFlags(cmd.Flags(), flags.SSHHosts, flags.SSHAcceptNew)
			return runInventory(cmd.Context(), flags)
		},
	}

	bindInventoryFlags(cmd, flags)

	// Source-scoped subcommands: same flags with a fixed --source value.
	helmCmd := &cobra.Command{
		Use:   "helm",
		Short: "Inventory Helm releases only (same as --source helm)",
		RunE: func(cmd *cobra.Command, args []string) error {
			applyGitSourceAuthFlags(cmd.Flags(), flags.SSHHosts, flags.SSHAcceptNew)
			flags.Source = inventory.SourceHelm
			return runInventory(cmd.Context(), flags)
		},
	}
	bindInventoryFlags(helmCmd, flags)
	cmd.AddCommand(helmCmd)

	crsCmd := &cobra.Command{
		Use:   "crs",
		Short: "Inventory operator custom resources only (same as --source cr)",
		RunE: func(cmd *cobra.Command, args []string) error {
			applyGitSourceAuthFlags(cmd.Flags(), flags.SSHHosts, flags.SSHAcceptNew)
			flags.Source = inventory.SourceCR
			return runInventory(cmd.Context(), flags)
		},
	}
	bindInventoryFlags(crsCmd, flags)
	cmd.AddCommand(crsCmd)

	return cmd
}

func bindInventoryFlags(cmd *cobra.Command, flags *InventoryFlags) {
	cmd.Flags().StringVarP(&flags.Path, "path", "p", "", "Path to the cluster directory in the repository")
	cmd.Flags().StringVarP(&flags.Namespace, "namespace", "n", "", "Filter components by namespace")
	cmd.Flags().StringVar(&flags.Source, "source", "", "Comma-separated component sources: helm, manifest, cr, crd (default all)")
	cmd.Flags().StringVarP(&flags.Output, "output", "o", "table", "Output format: table or markdown")
	cmd.Flags().StringVar(&flags.Sort, "sort", inventory.DefaultSort, "Comma-separated sort keys: name, namespace, source, version")
	cmd.Flags().BoolVar(&flags.NoHeaders, "no-headers", false, "Flat data rows with a fixed schema (SOURCE, NAMESPACE, NAME, PARENT, SOFTWARE, VERSION, CHART, IMAGES) — no section titles, headers or summary; for scripts")
	cmd.Flags().StringVar(&flags.Rules, "rules", "", "CR version-extraction rules file; no rules ship in the binary — see docs/inventory-rules-example.yaml (default .fluxview/inventory-rules.yaml in the repo root when present)")
	cmd.Flags().BoolVar(&flags.AllCRs, "all-crs", false, "Also show custom resources without an extraction rule, with version unknown")
	cmd.Flags().BoolVar(&flags.SplitUmbrella, "split-umbrella", false, "Split an umbrella HelmRelease into one row per application (grouped by app.kubernetes.io/name in the rendered chart); version of each row is the application's image tag")
	cmd.Flags().StringVar(&flags.BranchOrig, "branch-orig", "", "Compare against this git revision and add a CHANGE column (+ added, - removed, ~ old → new updated, = unchanged); exit code 1 when versions changed")
	registerHelmCacheFlags(cmd, &flags.HelmCache.dir, &flags.HelmCache.indexTTL, &flags.HelmCache.downloadTimeout)
	registerKustomizeCacheFlags(cmd, &flags.KsCache.remoteDir, &flags.KsCache.remoteTtl, &flags.KsCache.remoteTimeout, &flags.KsCache.buildCacheDir, &flags.KsCache.buildCacheTTL, &flags.KsCache.gitSourceDir, &flags.KsCache.gitSourceTtl)
	registerGitSourceAuthFlags(cmd, &flags.SSHHosts, &flags.SSHAcceptNew)
	registerNoGitSourceFetchFlag(cmd, &flags.NoFetch)
}

// parseSourceFilter parses the --source flag into a source set. Empty means
// all sources.
func parseSourceFilter(spec string) (map[string]bool, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	valid := map[string]bool{
		inventory.SourceHelm:     true,
		inventory.SourceManifest: true,
		inventory.SourceCR:       true,
		inventory.SourceCRD:      true,
	}
	out := map[string]bool{}
	for _, s := range strings.Split(spec, ",") {
		s = strings.TrimSpace(s)
		if !valid[s] {
			return nil, fmt.Errorf("unknown source %q (use helm, manifest, cr or crd)", s)
		}
		out[s] = true
	}
	return out, nil
}

func runInventory(ctx context.Context, flags *InventoryFlags) error {
	sortKeys, err := inventory.ParseSort(flags.Sort)
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}
	if flags.Output != "table" && flags.Output != "markdown" {
		return NewExitError(fmt.Errorf("unsupported output format %q (use 'table' or 'markdown')", flags.Output), ExitCodeError)
	}
	sources, err := parseSourceFilter(flags.Source)
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}

	clusterPath := flags.Path
	if clusterPath == "" {
		clusterPath = "."
	}
	absClusterPath, err := filepath.Abs(clusterPath)
	if err != nil {
		return NewExitError(fmt.Errorf("resolving path %s: %w", clusterPath, err), ExitCodeError)
	}
	if _, err := os.Stat(absClusterPath); os.IsNotExist(err) {
		return NewExitError(fmt.Errorf("path %s does not exist", clusterPath), ExitCodeError)
	}
	repoRoot, err := git.FindRepoRoot(absClusterPath)
	if err != nil {
		return NewExitError(fmt.Errorf("finding git repo root for %s: %w", clusterPath, err), ExitCodeError)
	}

	scans := newScanCache()
	gitEnv := newGitSourceEnv(ctx, repoRoot, flags.KsCache, flags.NoFetch)
	defer gitEnv.Close()

	inflater, err := helm.NewInflater(flags.HelmCache.inflaterOptions()...)
	if err != nil {
		return NewExitError(fmt.Errorf("initializing helm: %w", err), ExitCodeError)
	}
	defer inflater.Close()

	rules, err := loadInventoryRules(repoRoot, flags.Rules)
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}
	if rules.Len() == 0 {
		fmt.Fprintln(os.Stderr, "Note: no CR rules configured — CRs are hidden; add .fluxview/inventory-rules.yaml in the repo root or pass --rules (see docs/inventory-rules-example.yaml), or use --all-crs to list CRs with version unknown")
	}

	fleet, err := discoverHelmFleet(ctx, scans, absClusterPath, repoRoot, false, flags.KsCache, gitEnv)
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}
	if fleet == nil {
		return NewExitError(fmt.Errorf("no Flux Kustomization resources found in %s", clusterPath), ExitCodeError)
	}

	rows, components, err := collectFleetComponents(ctx, inflater, fleet, repoRoot, rules, flags.AllCRs, false, scans, gitEnv)
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}

	// Enrichment (render-driven: IMAGES, chart-rendered CRs, umbrella
	// splitting) runs BEFORE filtering and diffing, so that filters and
	// --branch-orig see the full, symmetric component set.
	currentFailed, currentSplit, err := enrichHelmRows(ctx, inflater, fleet, repoRoot, rows, &components, rules, flags.SplitUmbrella, false)
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}

	changes := false
	if flags.BranchOrig != "" {
		gitOps, err := git.NewOperations(repoRoot)
		if err != nil {
			return NewExitError(fmt.Errorf("opening git repo at %s: %w", repoRoot, err), ExitCodeError)
		}
		compare, compareFailed, compareSplit, err := collectAtRevision(ctx, inflater, gitOps, scans, absClusterPath, repoRoot, flags, rules, gitEnv)
		if err != nil {
			return NewExitError(err, ExitCodeError)
		}
		components = inventory.DiffComponents(compare, components, sortKeys)
		// A release whose chart failed to render on either side drops its
		// chart-derived rows from both sides — the diff must not report
		// rows one side simply could not render. A release that was split
		// on one side loses its own row too (shape alignment). The
		// metadata-based row of an unsplit release stays.
		components = dropChartDerived(components,
			append(append([]string{}, currentFailed...), compareFailed...),
			append(append([]string{}, currentSplit...), compareSplit...))
		changes = inventory.HasChanges(components)
	}

	components = inventory.Filter(components, inventory.FilterOptions{
		Namespace: flags.Namespace,
		Sources:   sources,
	})

	inventory.Sort(components, sortKeys)

	// Warnings go to stderr so stdout stays redirectable (see --output).
	for _, c := range components {
		for _, w := range c.Warnings {
			fmt.Fprintf(os.Stderr, "Warning: %s/%s: %s\n", c.Namespace, c.Name, w)
		}
	}

	renderOpts := inventory.RenderOptions{
		Headers: !flags.NoHeaders,
		Changes: flags.BranchOrig != "",
	}
	if flags.Output == "markdown" {
		fmt.Fprint(os.Stdout, inventory.Markdown(components, renderOpts))
	} else {
		fmt.Fprint(os.Stdout, inventory.Table(components, renderOpts))
	}

	if changes {
		return NewExitError(errors.New("inventory changed relative to "+flags.BranchOrig), ExitDiffFound)
	}
	return nil
}

// collectFleetComponents assembles every component source from one fleet:
// helm rows, operator CRs, manifest workloads and CRD groups (only groups
// with a known version). Returns the helm rows too — enrichment needs
// their resolved chart context for rendering. quiet suppresses per-component
// stderr diagnostics (the --branch-orig comparison side).
func collectFleetComponents(ctx context.Context, inflater *helm.Inflater, fleet *helmFleet, repoRoot string, rules *inventory.RuleSet, allCRs, quiet bool, scans *scanCache, gitEnv *gitSourceEnv) ([]helmRow, []inventory.Component, error) {
	rows, err := collectHelmComponents(ctx, inflater, fleet, repoRoot, quiet)
	if err != nil {
		return nil, nil, err
	}

	components := make([]inventory.Component, 0, len(rows))
	for _, row := range rows {
		components = append(components, row.component)
	}
	// One parse pass feeds every fleet-output collector (CRs, manifest
	// workloads, CRDs) — splitting and YAML-parsing the output separately
	// in each would triple the parse work (sixfold under --branch-orig).
	docs := fleetDocs(fleet)
	components = append(components, collectCRComponents(docs, rules, allCRs)...)
	components = append(components, inventory.CollectManifestComponents(manifestInputs(fleet, docs))...)
	components = append(components, inventory.CRDComponents(crdRaws(docs),
		crdGitSourceVersions(ctx, scans, repoRoot, gitEnv, quiet))...)
	return rows, components, nil
}

// fleetDocs parses the fleet build output into raw mappings, one per
// document. Empty and unparseable documents are skipped.
func fleetDocs(fleet *helmFleet) []map[string]interface{} {
	var docs []map[string]interface{}
	for _, doc := range flux.SplitYAMLText(fleet.output) {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" {
			continue
		}
		var raw map[string]interface{}
		if err := yaml.Unmarshal([]byte(trimmed), &raw); err != nil || raw == nil {
			continue
		}
		docs = append(docs, raw)
	}
	return docs
}

// collectAtRevision builds the component snapshot at a git revision
// (--branch-orig comparison side): checks the revision out into a temp
// worktree and runs the same collection quietly (the current-state side
// already reported the shared warnings).
func collectAtRevision(ctx context.Context, inflater *helm.Inflater, gitOps *git.Operations, scans *scanCache, clusterPath, repoRoot string, flags *InventoryFlags, rules *inventory.RuleSet, gitEnv *gitSourceEnv) (components []inventory.Component, failedParents, splitParents []string, err error) {
	worktreePath, err := gitOps.CloneToDir(ctx, flags.BranchOrig)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating worktree at %s: %w", flags.BranchOrig, err)
	}
	defer gitOps.RemoveWorktree(ctx, worktreePath)

	relPath, err := filepath.Rel(repoRoot, clusterPath)
	if err != nil {
		relPath = clusterPath
	}
	worktreeClusterPath := filepath.Join(worktreePath, relPath)
	if _, err := os.Stat(worktreeClusterPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "Warning: path %s does not exist at revision %s, treating as empty\n", relPath, flags.BranchOrig)
		return nil, nil, nil, nil
	}

	fleet, err := discoverHelmFleet(ctx, scans, worktreeClusterPath, worktreePath, true, flags.KsCache, gitEnv)
	if err != nil {
		return nil, nil, nil, err
	}
	if fleet == nil {
		return nil, nil, nil, nil
	}

	rows, components, err := collectFleetComponents(ctx, inflater, fleet, worktreePath, rules, flags.AllCRs, true, scans, gitEnv)
	if err != nil {
		return nil, nil, nil, err
	}
	// The comparison side enriches the same way as the current side
	// (rendered images, chart-rendered CRs, umbrella splitting) so the
	// diff is symmetric; quiet suppresses the duplicate progress line.
	failedParents, splitParents, err = enrichHelmRows(ctx, inflater, fleet, worktreePath, rows, &components, rules, flags.SplitUmbrella, true)
	if err != nil {
		return nil, failedParents, splitParents, err
	}
	return components, failedParents, splitParents, nil
}

// crdRaws collects CustomResourceDefinition documents from the parsed build
// output (see fleetDocs).
func crdRaws(docs []map[string]interface{}) []map[string]interface{} {
	var raws []map[string]interface{}
	for _, raw := range docs {
		if kind, _ := raw["kind"].(string); kind == "CustomResourceDefinition" {
			raws = append(raws, raw)
		}
	}
	return raws
}

// crdGitSourceVersions maps CRD API groups to the pinned ref tag of the
// external GitRepository whose clone ships them (e.g. a kyverno-crds
// Kustomization backed by GitRepository kyverno at tag v1.19.1). Only
// pinned tags are versions; branch/HEAD refs and same-repo (local) sources
// are skipped. Repositories are processed in sorted name order, so the
// first writer per group wins deterministically.
func crdGitSourceVersions(ctx context.Context, scans *scanCache, repoRoot string, gitEnv *gitSourceEnv, quiet bool) map[string]string {
	if gitEnv == nil || gitEnv.originURL == "" {
		return nil
	}
	repos, err := scans.parserFor(repoRoot).ParseGitRepositories(ctx)
	if err != nil || len(repos) == 0 {
		return nil
	}

	index := indexByNSName(repos, func(r flux.GitRepository) flux.ObjectMeta { return r.Metadata })
	keys := make([]string, 0, len(index))
	for k := range index {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	groupVersions := map[string]string{}
	for _, key := range keys {
		gr := index[key]
		if git.SameGitRepo(gr.Spec.URL, gitEnv.originURL) {
			continue // local source, not an external CRD upstream
		}
		version := ""
		if gr.Spec.Ref != nil {
			version = gr.Spec.Ref.Tag
		}
		if version == "" {
			continue // floating ref (branch/HEAD/semver) — not a version
		}
		cloneDir, err := gitEnv.ensure(ctx, gr)
		if err != nil {
			if !quiet {
				fmt.Fprintf(os.Stderr, "Warning: could not read CRD source %s (%s): %v\n", gr.Spec.URL, version, err)
			}
			continue
		}
		for group := range crdGroupsIn(cloneDir) {
			if _, exists := groupVersions[group]; !exists {
				groupVersions[group] = version
			}
		}
	}
	return groupVersions
}

// crdGroupsIn walks a clone for CustomResourceDefinition documents and
// returns the set of their API groups. Files are pre-filtered with a byte
// search before the YAML parse — upstream clones hold thousands of
// unrelated manifests, and full parsing of each dominates the run.
func crdGroupsIn(dir string) map[string]bool {
	const crdMarker = "CustomResourceDefinition"
	groups := map[string]bool{}
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Contains(data, []byte(crdMarker)) {
			return nil
		}
		for _, doc := range flux.SplitYAMLText(data) {
			trimmed := strings.TrimSpace(doc)
			if trimmed == "" {
				continue
			}
			var crd struct {
				Kind string `yaml:"kind"`
				Spec struct {
					Group string `yaml:"group"`
				} `yaml:"spec"`
			}
			if err := yaml.Unmarshal([]byte(trimmed), &crd); err != nil || crd.Kind != crdMarker || crd.Spec.Group == "" {
				continue
			}
			groups[crd.Spec.Group] = true
		}
		return nil
	})
	return groups
}

// enrichHelmRows renders the chart of every resolvable helm row and mines
// the rendered manifest for everything the helm section cannot know from
// metadata alone:
//
//   - the IMAGES column (all workload images of the chart);
//   - CRs the chart creates — they become cr components with the Parent
//     field pointing at the HelmRelease (deduped against fleet-collected
//     CRs by identity);
//   - umbrella splitting (splitUmbrella): when the rendered workloads form
//     more than one application group (by app.kubernetes.io/name, falling
//     back to part-of, then the release name), the single chart row is
//     replaced by one row per application, version = the application's
//     main-container image tag.
//
// Rendering failures degrade to a warning — the component keeps its
// metadata-only row — and the release name is returned in failedParents:
// chart-derived rows (chart-rendered CRs, umbrella children) participate in
// the --branch-orig diff by identity, so a release that fails to render on
// one side only would fabricate +/- rows; the caller drops its derived rows
// from BOTH sides. A split release replaces its own helm row with children,
// so a failure/split combination drops the parent row too — both sides must
// present the same row shape or the diff fabricates lines. The second
// return value lists the split releases (namespace-qualified); the
// metadata-based row of an UNSPLIT failed release stays with its warning.
// A context cancellation aborts with the error. quiet suppresses the
// progress line (the --branch-orig comparison side).
func enrichHelmRows(ctx context.Context, inflater *helm.Inflater, fleet *helmFleet, repoRoot string, rows []helmRow, components *[]inventory.Component, rules *inventory.RuleSet, splitUmbrella, quiet bool) (failedParents, splitParents []string, err error) {
	renderable := 0
	for _, row := range rows {
		if row.renderable {
			renderable++
		}
	}
	if renderable > 0 && !quiet {
		fmt.Fprintf(os.Stderr, "Rendering %d HelmRelease chart(s) for images\n", renderable)
	}

	// Identity index of already-collected CRs (from the fleet output) —
	// O(1) lookups for chart-rendered CR dedup.
	knownCR := map[string]bool{}
	for i := range *components {
		if c := (*components)[i]; c.Source == inventory.SourceCR {
			knownCR[c.Kind+"/"+c.Namespace+"/"+c.Name] = true
		}
	}

	seenCR := map[string]bool{}
	for i := range rows {
		row := rows[i]
		if !row.renderable {
			continue
		}
		if err := CheckInterrupted(ctx); err != nil {
			return failedParents, splitParents, err
		}

		rendered, err := inflater.InflateHelmRelease(ctx, row.hr, row.repoURL, row.username, row.password, fleet.configMaps, fleet.secrets, repoRoot)
		idx := findHelmComponent(*components, row.component.Namespace, row.component.Name)
		if idx < 0 {
			continue
		}
		if err != nil {
			(*components)[idx].Warnings = append((*components)[idx].Warnings, fmt.Sprintf("could not render images: %v", err))
			failedParents = append(failedParents, parentKey(row.component.Namespace, row.component.Name))
			continue
		}

		docs := renderedDocs(rendered)
		*components = chartCRs(*components, docs, row, rules, seenCR, knownCR)

		if splitUmbrella {
			if children, ok := umbrellaChildren((*components)[idx], docs); ok {
				*components = append((*components)[:idx], (*components)[idx+1:]...)
				*components = append(*components, children...)
				splitParents = append(splitParents, parentKey(row.component.Namespace, row.component.Name))
				continue
			}
		}
		(*components)[idx].Images = inventory.ImagesFromDocs(rawsOfDocs(docs))
	}
	return failedParents, splitParents, nil
}

// parentKey is the namespace-qualified identity of a parent HelmRelease —
// two releases may share a name across namespaces.
func parentKey(namespace, name string) string { return namespace + "/" + name }

// dropChartDerived restores the symmetry of a render failure on either
// --branch-orig side, so the diff cannot report rows one side simply could
// not render:
//
//   - chart-derived rows (Parent set, namespace-qualified) of a failed
//     release drop from BOTH sides — same-named releases in different
//     namespaces never over-drop each other;
//   - a release that failed on one side and was SPLIT on the other also
//     drops its own helm row: the split replaced the row with children on
//     one side while the failed side kept it, and the shape mismatch would
//     fabricate a +/- line. An unsplit failed release keeps its
//     metadata-based row (with the render warning) on both sides.
func dropChartDerived(components []inventory.Component, failed, split []string) []inventory.Component {
	if len(failed) == 0 && len(split) == 0 {
		return components
	}
	drop := make(map[string]bool, len(failed))
	for _, name := range failed {
		drop[name] = true
	}
	shapeMismatch := make(map[string]bool)
	for _, name := range split {
		if drop[name] {
			shapeMismatch[name] = true
		}
	}

	kept := components[:0]
	for _, c := range components {
		if c.Parent != "" && drop[c.Parent] {
			continue
		}
		if c.Source == inventory.SourceHelm && c.Parent == "" && shapeMismatch[parentKey(c.Namespace, c.Name)] {
			continue
		}
		kept = append(kept, c)
	}
	return kept
}

// findHelmComponent locates a helm component by release namespace and name.
func findHelmComponent(components []inventory.Component, namespace, name string) int {
	for i := range components {
		if components[i].Source == inventory.SourceHelm &&
			components[i].Namespace == namespace &&
			components[i].Name == name {
			return i
		}
	}
	return -1
}

// renderedDoc is one parsed document of a rendered chart.
type renderedDoc struct {
	apiVersion string
	kind       string
	raw        map[string]interface{}
}

// renderedDocs parses a rendered chart manifest into documents.
func renderedDocs(rendered []byte) []renderedDoc {
	var docs []renderedDoc
	for _, doc := range flux.SplitYAMLText(rendered) {
		trimmed := strings.TrimSpace(doc)
		if trimmed == "" {
			continue
		}
		var raw map[string]interface{}
		if err := yaml.Unmarshal([]byte(trimmed), &raw); err != nil || raw == nil {
			continue
		}
		apiVersion, _ := raw["apiVersion"].(string)
		kind, _ := raw["kind"].(string)
		docs = append(docs, renderedDoc{apiVersion: apiVersion, kind: kind, raw: raw})
	}
	return docs
}

func rawsOfDocs(docs []renderedDoc) []map[string]interface{} {
	raws := make([]map[string]interface{}, 0, len(docs))
	for _, d := range docs {
		raws = append(raws, d.raw)
	}
	return raws
}

// chartCRs appends cr components for the CRs a chart renders, with Parent
// pointing at the HelmRelease. Rule-less chart CRs stay hidden; identities
// already collected (fleet CRs — the git source of truth — or the same CR
// emitted twice) are skipped via the knownCR index.
func chartCRs(components []inventory.Component, docs []renderedDoc, row helmRow, rules *inventory.RuleSet, seen, knownCR map[string]bool) []inventory.Component {
	for _, doc := range docs {
		if !inventory.IsCRCandidate(doc.apiVersion) {
			continue
		}
		c, ok, key := crComponentFromRaw(doc.raw, rules, false)
		if !ok || seen[key] {
			continue
		}
		seen[key] = true
		known := c.Kind + "/" + c.Namespace + "/" + c.Name
		if knownCR[known] {
			continue
		}
		knownCR[known] = true
		c.Parent = parentKey(row.component.Namespace, row.component.Name)
		components = append(components, c)
	}
	return components
}

// umbrellaChildren splits a chart row into one row per application when the
// rendered workloads form more than one application group. Grouping follows
// the manifest convention: app.kubernetes.io/name → part-of → the release
// name. Each child carries the parent chart, the application's images and
// its main-container image tag as the version; the parent's warnings are
// inherited (they describe the same chart).
func umbrellaChildren(parent inventory.Component, docs []renderedDoc) ([]inventory.Component, bool) {
	type group struct {
		name string
		raws []map[string]interface{}
	}
	groups := map[string]*group{}
	order := []string{}
	for _, doc := range docs {
		if !inventory.IsWorkload(doc.kind) {
			continue
		}
		app := ""
		if metadata, ok := doc.raw["metadata"].(map[string]interface{}); ok {
			if labels, ok := metadata["labels"].(map[string]interface{}); ok {
				if v, ok := labels["app.kubernetes.io/name"].(string); ok && v != "" {
					app = v
				} else if v, ok := labels["app.kubernetes.io/part-of"].(string); ok && v != "" {
					app = v
				}
			}
		}
		if app == "" {
			app = parent.Name
		}
		g, exists := groups[app]
		if !exists {
			g = &group{name: app}
			groups[app] = g
			order = append(order, app)
		}
		g.raws = append(g.raws, doc.raw)
	}
	if len(groups) <= 1 {
		return nil, false
	}

	children := make([]inventory.Component, 0, len(groups))
	for _, app := range order {
		g := groups[app]
		child := inventory.Component{
			Source: inventory.SourceHelm,
			Chart:  parent.Chart,
			Parent: parentKey(parent.Namespace, parent.Name),
			Images: inventory.ImagesFromDocs(g.raws),
			FluxKs: parent.FluxKs,
			Path:   parent.Path,
		}
		child.Name = parent.Name + "/" + g.name
		child.Software = g.name
		// Copy: children share the parent's chart warnings, and a later
		// append on one child must never write through a shared backing
		// array into a sibling.
		child.Warnings = append([]string(nil), parent.Warnings...)
		if v, source, warning := inventory.WorkloadGroupVersion(g.raws, g.name); v != "" {
			child.Version = v
			child.VersionSource = source
			if warning != "" {
				child.Warnings = append(child.Warnings, warning)
			}
		} else {
			child.Version = inventory.UnknownVersion
		}
		child.Kind = inventory.WorkloadGroupKind(g.raws)
		if ns := inventory.WorkloadGroupNamespace(g.raws); ns != "" {
			child.Namespace = ns
		} else {
			child.Namespace = parent.Namespace
		}
		children = append(children, child)
	}
	return children, true
}

// manifestInputs prepares workload documents for grouping, with Flux
// Kustomization attribution: the kustomize.toolkit.fluxcd.io/name label
// names the KS, whose spec.path becomes the component's path. docs is the
// parsed build output (see fleetDocs).
func manifestInputs(fleet *helmFleet, docs []map[string]interface{}) []inventory.ManifestInput {
	ksPathByName := make(map[string]string, len(fleet.kustomizations))
	for _, ks := range fleet.kustomizations {
		if _, seen := ksPathByName[ks.Metadata.Name]; !seen && ks.Spec.Path != "" {
			ksPathByName[ks.Metadata.Name] = ks.Spec.Path
		}
	}

	var inputs []inventory.ManifestInput
	for _, raw := range docs {
		if kind, _ := raw["kind"].(string); !inventory.IsWorkload(kind) {
			continue
		}
		in := inventory.ManifestInput{Raw: raw}
		if metadata, ok := raw["metadata"].(map[string]interface{}); ok {
			if labels, ok := metadata["labels"].(map[string]interface{}); ok {
				if ks, ok := labels["kustomize.toolkit.fluxcd.io/name"].(string); ok {
					in.FluxKs = ks
					in.Path = ksPathByName[ks]
				}
			}
		}
		inputs = append(inputs, in)
	}
	return inputs
}

// helmRow pairs one helm component with its resolved chart context, so
// enrichment can render the chart later without repeating source
// resolution.
type helmRow struct {
	component inventory.Component
	hr        flux.HelmRelease // post-resolution: chart path / OCI ref filled in
	repoURL   string
	username  string
	password  string
	// renderable reports that the chart resolved during collection; only
	// then can templates render for the IMAGES column. Rows with an
	// unresolved chart keep their single warning instead of collecting a
	// second one from a doomed render.
	renderable bool
}

// collectHelmComponents builds one component per HelmRelease in the fleet:
// chart name and resolved version, appVersion from Chart.yaml, the chart
// source. Chart resolution failures degrade to a warning and a row
// with the spec version — they never abort the inventory. A context
// cancellation aborts the collection and returns the error.
func collectHelmComponents(ctx context.Context, inflater *helm.Inflater, fleet *helmFleet, repoRoot string, quiet bool) ([]helmRow, error) {
	ociRepoIndex := indexByNSName(fleet.ociRepos, func(r flux.OCIRepository) flux.ObjectMeta { return r.Metadata })
	helmRepoIndex := indexByNSName(fleet.helmRepos, func(r flux.HelmRepository) flux.ObjectMeta { return r.Metadata })
	secretIndex := indexByNSName(fleet.secrets, func(s flux.Secret) flux.ObjectMeta { return s.Metadata })
	gitRepoIndex := indexByNSName(fleet.gitRepos, func(r flux.GitRepository) flux.ObjectMeta { return r.Metadata })

	rows := make([]helmRow, 0, len(fleet.helmReleases))
	for _, hr := range fleet.helmReleases {
		if err := CheckInterrupted(ctx); err != nil {
			return rows, err
		}

		if hr.Spec.Suspend {
			if !quiet {
				fmt.Fprintf(os.Stderr, "Warning: HelmRelease %s/%s is suspended, skipping\n",
					hr.Metadata.Namespace, hr.Metadata.Name)
			}
			continue
		}

		row := helmRow{hr: hr}
		c := &row.component
		c.Source = inventory.SourceHelm
		c.Kind = flux.KindHelmRelease
		c.Namespace = hr.Metadata.Namespace
		c.Name = hr.Metadata.Name
		chartInfo := &inventory.ChartInfo{
			Version: hr.Spec.Chart.Spec.Version,
		}
		c.Chart = chartInfo

		meta, res, metaOK := resolveHRChartMeta(ctx, inflater, hr, repoRoot, chartInfo, ociRepoIndex, helmRepoIndex, secretIndex, gitRepoIndex, c)
		row.hr = res.hr
		row.repoURL = res.repoURL
		row.username = res.username
		row.password = res.password
		row.renderable = metaOK

		switch {
		case metaOK && meta.Name != "":
			c.Software = meta.Name
		default:
			c.Software = chartInfo.Name
		}
		if metaOK {
			chartInfo.Version = meta.Version
			if hr.Spec.Chart.Spec.Version != "" && hr.Spec.Chart.Spec.Version != meta.Version {
				chartInfo.Constraint = hr.Spec.Chart.Spec.Version
			}
			c.Version = meta.AppVersion
			c.VersionSource = inventory.VersionAppVersion
		}
		if c.Version == "" {
			c.Version = "-"
			if !metaOK && len(c.Warnings) == 0 {
				c.Warnings = append(c.Warnings, "appVersion unknown (chart metadata unavailable)")
			}
		}

		rows = append(rows, row)
	}
	return rows, nil
}

// chartResolution carries what resolveHRChartMeta resolved, so enrichment can
// render the chart later without repeating source resolution.
type chartResolution struct {
	hr       flux.HelmRelease // chart path / OCI ref / version filled in
	repoURL  string
	username string
	password string
}

// resolveHRChartMeta resolves the chart source of one HelmRelease and reads
// its Chart.yaml metadata. It fills chartInfo (source kind/URL/name) as it
// goes and records warnings on the component when the chart cannot be
// resolved (the row still shows the spec chart version). The bool
// result reports whether the metadata was read.
//
// hr is a value copy from the caller's range loop, so the GitRepository-path
// rewrite below (same as inflateHelmReleasesShared) is local.
func resolveHRChartMeta(ctx context.Context, inflater *helm.Inflater, hr flux.HelmRelease, repoRoot string, chartInfo *inventory.ChartInfo, ociRepoIndex map[string]flux.OCIRepository, helmRepoIndex map[string]flux.HelmRepository, secretIndex map[string]flux.Secret, gitRepoIndex map[string]flux.GitRepository, c *inventory.Component) (helm.ChartMeta, chartResolution, bool) {
	chartSpec := hr.Spec.Chart.Spec
	chartInfo.Name = chartSpec.Chart
	chartInfo.SourceKind = chartSpec.SourceRef.Kind
	res := chartResolution{hr: hr}

	warn := func(format string, args ...any) {
		c.Warnings = append(c.Warnings, fmt.Sprintf(format, args...))
	}

	switch {
	case hr.Spec.ChartRef != nil && hr.Spec.ChartRef.Kind == flux.KindOCIRepository:
		repoNS := hr.Spec.ChartRef.Namespace
		if repoNS == "" {
			repoNS = hr.Metadata.Namespace
		}
		repo, ok := ociRepoIndex[repoNS+"/"+hr.Spec.ChartRef.Name]
		if ok {
			chartInfo.SourceURL = repo.Spec.URL
		}
		ociRef, ociVersion := resolveOCIRepoURL(hr, ociRepoIndex)
		if ociRef == "" {
			warn("could not resolve OCIRepository source (chartRef %s/%s) — not found",
				hr.Spec.ChartRef.Namespace, hr.Spec.ChartRef.Name)
			return helm.ChartMeta{}, res, false
		}
		res.hr.Spec.Chart.Spec.Chart = ociRef
		res.hr.Spec.Chart.Spec.Version = ociVersion
		chartInfo.Version = ociVersion
		if chartInfo.Name == "" {
			chartInfo.Name = path.Base(strings.TrimSuffix(repo.Spec.URL, "/"))
		}
	case chartSpec.Chart == "":
		// Includes chartRef.kind=HelmChart (documented limitation).
		warn("no chart name (chartRef kind %q is not supported)", chartKindOf(hr))
		return helm.ChartMeta{}, res, false
	case chartSpec.SourceRef.Kind == flux.KindGitRepository:
		// The chart lives in the local checkout: resolve as a directory path
		// (no network), mirroring inflateHelmReleasesShared.
		resolved, err := securejoin.SecureJoin(repoRoot, chartSpec.Chart)
		if err != nil {
			warn("cannot safely resolve chart path %s: %v", chartSpec.Chart, err)
			return helm.ChartMeta{}, res, false
		}
		if _, err := os.Stat(resolved); err != nil {
			warn("chart %q not found locally (%s source)", chartSpec.Chart, chartSpec.SourceRef.Kind)
			return helm.ChartMeta{}, res, false
		}
		res.hr.Spec.Chart.Spec.Chart = resolved
		chartInfo.Name = path.Base(strings.TrimSuffix(chartSpec.Chart, "/"))
		repoNS := chartSpec.SourceRef.Namespace
		if repoNS == "" {
			repoNS = hr.Metadata.Namespace
		}
		if repo, ok := gitRepoIndex[repoNS+"/"+chartSpec.SourceRef.Name]; ok {
			chartInfo.SourceURL = repo.Spec.URL
		}
	case chartSpec.SourceRef.Kind == flux.KindBucket:
		// Chart unavailable for Bucket sources — keep the spec version.
		warn("chart unavailable (Bucket source is not supported)")
		return helm.ChartMeta{}, res, false
	default:
		res.repoURL, res.username, res.password = resolveHelmRepoURL(hr, helmRepoIndex, secretIndex)
		if res.repoURL == "" {
			warn("could not resolve source (chart %q) — HelmRepository not found", chartSpec.Chart)
			return helm.ChartMeta{}, res, false
		}
		chartInfo.SourceURL = res.repoURL
	}

	meta, err := inflater.ChartMeta(ctx, res.hr, res.repoURL, res.username, res.password)
	if err != nil {
		warn("chart unavailable: %v", err)
		return helm.ChartMeta{}, res, false
	}
	return meta, res, true
}

// chartKindOf names a chartRef-based HelmRelease's referenced kind, for
// warnings on unsupported chartRef kinds.
func chartKindOf(hr flux.HelmRelease) string {
	if hr.Spec.ChartRef != nil && hr.Spec.ChartRef.Kind != "" {
		return hr.Spec.ChartRef.Kind
	}
	return "none"
}

// defaultRulesPath is the conventional location of user rules inside the
// repository.
const defaultRulesPath = ".fluxview/inventory-rules.yaml"

// loadInventoryRules builds the effective rule set from user rules only:
// `--rules` or, when not set, .fluxview/inventory-rules.yaml in the
// repository root when present. No rules ship inside the binary — operator
// fields differ per cluster and drift over time, so the rules are the
// user's to own (docs/inventory-rules-example.yaml in the fluxview repo is
// a copyable starting point). An explicitly passed unreadable file is an
// error; the conventional file simply may not exist.
func loadInventoryRules(repoRoot, rulesFlag string) (*inventory.RuleSet, error) {
	userRulesPath := rulesFlag
	if userRulesPath == "" {
		candidate := filepath.Join(repoRoot, defaultRulesPath)
		if _, err := os.Stat(candidate); err == nil {
			userRulesPath = candidate
		}
	}

	var rules []inventory.Rule
	if userRulesPath != "" {
		data, err := os.ReadFile(userRulesPath)
		if err != nil {
			return nil, fmt.Errorf("reading rules file %s: %w", userRulesPath, err)
		}
		parsed, err := inventory.ParseRuleFile(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", userRulesPath, err)
		}
		rules = parsed
	}
	return inventory.NewRuleSet(rules)
}

// collectCRComponents walks the Kustomization build output and turns every
// custom resource matching an extraction rule into a cr component.
// Rule-less CRs are skipped unless allCRs, in which case they are listed
// with version unknown. A matched rule whose version fields all came back
// empty yields "default" — the operator's built-in default version — with
// a warning marker.
func collectCRComponents(docs []map[string]interface{}, rules *inventory.RuleSet, allCRs bool) []inventory.Component {
	var components []inventory.Component
	seen := map[string]bool{}
	for _, raw := range docs {
		c, ok, key := crComponentFromRaw(raw, rules, allCRs)
		if !ok {
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		components = append(components, c)
	}
	return components
}

// crIdentity keys a CR document for dedup across collection paths.
func crIdentity(apiVersion, namespace, kind, name string) string {
	return apiVersion + "/" + namespace + "/" + kind + "/" + name
}

// crComponentFromRaw builds one cr component from a CR document: rule
// extraction (version, image) when the rule matches, `unknown` under
// allowUnknown (the --all-crs path for fleet-collected CRs; chart-rendered
// CRs always require a rule). The third result is the dedup identity.
func crComponentFromRaw(raw map[string]interface{}, rules *inventory.RuleSet, allowUnknown bool) (inventory.Component, bool, string) {
	apiVersion, _ := raw["apiVersion"].(string)
	kind, _ := raw["kind"].(string)
	if apiVersion == "" || kind == "" || !inventory.IsCRCandidate(apiVersion) {
		return inventory.Component{}, false, ""
	}
	metadata, _ := raw["metadata"].(map[string]interface{})
	if metadata == nil {
		return inventory.Component{}, false, ""
	}
	name, _ := metadata["name"].(string)
	if name == "" {
		return inventory.Component{}, false, ""
	}
	namespace, _ := metadata["namespace"].(string)

	c := inventory.Component{
		Source:    inventory.SourceCR,
		Kind:      kind,
		Namespace: namespace,
		Name:      name,
	}
	// Cut, not SplitN: Lookup needs the group only, and Cut allocates none.
	group, _, _ := strings.Cut(apiVersion, "/")
	rule, matched := rules.Lookup(group, kind)
	if !matched {
		if !allowUnknown {
			return inventory.Component{}, false, ""
		}
		c.Software = kind
		c.Version = inventory.UnknownVersion
		return c, true, crIdentity(apiVersion, namespace, kind, name)
	}

	c.Software = rule.Software
	c.Operator = rule.Operator
	if version, ok := rule.Extract(raw); ok {
		c.Version = version
		c.VersionSource = inventory.VersionCRField
	} else {
		c.Version = inventory.DefaultVersion
		c.VersionSource = inventory.VersionDefault
		c.Warnings = append(c.Warnings, "version not set in CR, operator default assumed")
	}
	if image, ok := rule.ExtractImage(raw); ok {
		c.Images = []string{image}
	}
	return c, true, crIdentity(apiVersion, namespace, kind, name)
}
