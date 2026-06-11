package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/banschikovde/flux-diff/internal/flux"
	"github.com/banschikovde/flux-diff/internal/git"
	"github.com/banschikovde/flux-diff/internal/helm"
	"github.com/banschikovde/flux-diff/internal/kustomize"
)

// secretCount tracks total redacted secrets across all outputs.
var secretCount int

// BuildFlags holds flags for the build command.
type BuildFlags struct {
	Path string
}

func newBuildCmd() *cobra.Command {
	flags := &BuildFlags{}

	cmd := &cobra.Command{
		Use:   "build <resource> [name] [flags]",
		Short: "Build (assemble) Flux Kustomization or HelmRelease resources",
		Long: `Build Flux resources from a local git repository.

Supported resource types:
  ks — Flux Kustomization: runs kustomize build and inflates HelmReleases
  hr — Flux HelmRelease: inflates Helm chart via helm template

Examples:
  flux-diff build ks --path clusters/prod/
  flux-diff build hr podinfo --path clusters/prod/`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBuild(cmd.Context(), args, flags)
		},
	}

	cmd.Flags().StringVarP(&flags.Path, "path", "p", "", "Path to the cluster directory in the repository")

	return cmd
}

func runBuild(ctx context.Context, args []string, flags *BuildFlags) error {
	resourceType := args[0]
	var name string
	if len(args) > 1 {
		name = args[1]
	}

	// Determine the repository root.
	repoRoot, err := git.FindRepoRoot(".")
	if err != nil {
		return NewExitError(fmt.Errorf("finding git repo root: %w", err), ExitCodeError)
	}

	// Determine the cluster path.
	clusterPath := flags.Path
	if clusterPath == "" {
		clusterPath = "."
	}
	absClusterPath := clusterPath
	if !filepath.IsAbs(clusterPath) {
		absClusterPath = filepath.Join(repoRoot, clusterPath)
	}

	// Verify the cluster path exists.
	if _, err := os.Stat(absClusterPath); os.IsNotExist(err) {
		return NewExitError(fmt.Errorf("path %s does not exist", clusterPath), ExitCodeError)
	}

	switch resourceType {
	case "ks", "kustomization":
		return runBuildKS(ctx, absClusterPath, repoRoot, name, flags)
	case "hr", "helmrelease":
		return runBuildHR(ctx, absClusterPath, repoRoot, name, flags)
	default:
		return NewExitError(fmt.Errorf("unsupported resource type %q (use 'ks' or 'hr')", resourceType), ExitCodeError)
	}
}

func runBuildKS(ctx context.Context, clusterPath, repoRoot, name string, flags *BuildFlags) error {
	fmt.Fprintf(os.Stderr, "Building Kustomization resources from %s...\n", clusterPath)

	// Parse Flux Kustomization resources from the cluster path.
	parser := flux.NewParser(clusterPath)
	kustomizations, err := parser.ParseKustomizations(ctx)
	if err != nil {
		return NewExitError(fmt.Errorf("parsing Kustomization resources: %w", err), ExitCodeError)
	}

	if len(kustomizations) == 0 {
		fmt.Fprintf(os.Stderr, "No Flux Kustomization resources found in %s\n", clusterPath)
		return nil
	}

	// Filter by name if specified.
	if name != "" {
		kustomizations = filterKustomizations(kustomizations, name)
		if len(kustomizations) == 0 {
			return NewExitError(fmt.Errorf("Kustomization %q not found", name), ExitCodeError)
		}
	}

	// Build the kustomize resources via SDK.
	builder := kustomize.NewBuilder()

	for _, ks := range kustomizations {
		if ks.Spec.Suspend {
			fmt.Fprintf(os.Stderr, "Skipping suspended Kustomization %s/%s\n", ks.Metadata.Namespace, ks.Metadata.Name)
			continue
		}

		// Resolve the source path.
		sourcePath := resolveSourcePath(repoRoot, ks)
		if sourcePath == "" {
			fmt.Fprintf(os.Stderr, "Warning: could not resolve source path for Kustomization %s/%s, skipping\n",
				ks.Metadata.Namespace, ks.Metadata.Name)
			continue
		}

		fmt.Fprintf(os.Stderr, "Building Kustomization %s/%s (path: %s)...\n",
			ks.Metadata.Namespace, ks.Metadata.Name, sourcePath)

		output, err := builder.Build(sourcePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error building Kustomization %s/%s: %v\n",
				ks.Metadata.Namespace, ks.Metadata.Name, err)
			continue
		}

		// Redact secrets and output to stdout.
		printRedacted(output)
	}

	// Inflate HelmReleases found in the cluster path.
	if err := inflateHelmReleases(ctx, clusterPath); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: helm inflation failed: %v\n", err)
	}

	reportSecretSummary()
	return nil
}

func runBuildHR(ctx context.Context, clusterPath, repoRoot, name string, flags *BuildFlags) error {
	if name == "" {
		return NewExitError(fmt.Errorf("HelmRelease name is required for 'build hr' command"), ExitCodeError)
	}

	fmt.Fprintf(os.Stderr, "Building HelmRelease %s in %s...\n", name, clusterPath)

	// Parse HelmRelease resources.
	parser := flux.NewParser(clusterPath)
	helmReleases, err := parser.ParseHelmReleases(ctx)
	if err != nil {
		return NewExitError(fmt.Errorf("parsing HelmRelease resources: %w", err), ExitCodeError)
	}

	// Filter by name.
	helmReleases = filterHelmReleases(helmReleases, name)
	if len(helmReleases) == 0 {
		return NewExitError(fmt.Errorf("HelmRelease %q not found", name), ExitCodeError)
	}

	// Parse HelmRepository resources for resolving chart URLs.
	helmRepos, err := parser.ParseHelmRepositories(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not parse HelmRepositories: %v\n", err)
	}

	// Create helm inflater (Go SDK, no external binary needed).
	inflater, err := helm.NewInflater()
	if err != nil {
		return NewExitError(fmt.Errorf("initializing helm: %w", err), ExitCodeError)
	}

	for _, hr := range helmReleases {
		if hr.Spec.Suspend {
			fmt.Fprintf(os.Stderr, "Skipping suspended HelmRelease %s/%s\n", hr.Metadata.Namespace, hr.Metadata.Name)
			continue
		}

		// Find the HelmRepository URL.
		repoURL := ""
		sourceRef := hr.Spec.Chart.Spec.SourceRef
		if sourceRef.Kind == flux.KindHelmRepository {
			repoNS := sourceRef.Namespace
			if repoNS == "" {
				repoNS = hr.Metadata.Namespace
			}
			url, err := helm.FindHelmRepoURL(helmRepos, sourceRef.Name, repoNS)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: %v\n", err)
			} else {
				repoURL = url
			}
		}

		fmt.Fprintf(os.Stderr, "Inflating HelmRelease %s/%s (chart: %s)...\n",
			hr.Metadata.Namespace, hr.Metadata.Name, hr.Spec.Chart.Spec.Chart)

		output, err := inflater.InflateHelmRelease(ctx, hr, repoURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error inflating HelmRelease %s/%s: %v\n",
				hr.Metadata.Namespace, hr.Metadata.Name, err)
			continue
		}

		// Redact secrets and output to stdout.
		printRedacted(output)
	}

	reportSecretSummary()
	return nil
}

// printRedacted redacts secrets from the output, prints to stdout,
// and reports the count to stderr.
func printRedacted(data []byte) {
	if data == nil {
		return
	}
	redacted := flux.RedactSecrets(data)
	count := flux.CountSecrets(data)
	secretCount += count
	fmt.Print(string(redacted))
}

// reportSecretSummary prints a summary of redacted secrets to stderr.
func reportSecretSummary() {
	if secretCount > 0 {
		fmt.Fprintf(os.Stderr, "%s\n", flux.FormatSecretSummary(secretCount))
	}
}

// inflateHelmReleases scans for HelmRelease resources in the cluster path and inflates them.
func inflateHelmReleases(ctx context.Context, clusterPath string) error {
	parser := flux.NewParser(clusterPath)
	helmReleases, err := parser.ParseHelmReleases(ctx)
	if err != nil {
		return fmt.Errorf("parsing HelmReleases: %w", err)
	}

	if len(helmReleases) == 0 {
		return nil
	}

	helmRepos, _ := parser.ParseHelmRepositories(ctx)

	inflater, err := helm.NewInflater()
	if err != nil {
		return fmt.Errorf("initializing helm: %w", err)
	}

	for _, hr := range helmReleases {
		if hr.Spec.Suspend {
			continue
		}

		repoURL := ""
		sourceRef := hr.Spec.Chart.Spec.SourceRef
		if sourceRef.Kind == flux.KindHelmRepository {
			repoNS := sourceRef.Namespace
			if repoNS == "" {
				repoNS = hr.Metadata.Namespace
			}
			url, err := helm.FindHelmRepoURL(helmRepos, sourceRef.Name, repoNS)
			if err != nil {
				continue
			}
			repoURL = url
		}

		output, err := inflater.InflateHelmRelease(ctx, hr, repoURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to inflate HelmRelease %s/%s: %v\n",
				hr.Metadata.Namespace, hr.Metadata.Name, err)
			continue
		}

		printRedacted(output)
	}

	return nil
}

// filterKustomizations filters Kustomization resources by name.
func filterKustomizations(resources []flux.Kustomization, name string) []flux.Kustomization {
	var result []flux.Kustomization
	for _, ks := range resources {
		if name != "" && ks.Metadata.Name != name {
			continue
		}
		result = append(result, ks)
	}
	return result
}

// filterHelmReleases filters HelmRelease resources by name.
func filterHelmReleases(resources []flux.HelmRelease, name string) []flux.HelmRelease {
	var result []flux.HelmRelease
	for _, hr := range resources {
		if name != "" && hr.Metadata.Name != name {
			continue
		}
		result = append(result, hr)
	}
	return result
}

// resolveSourcePath resolves the path for a Kustomization's source.
func resolveSourcePath(repoRoot string, ks flux.Kustomization) string {
	if ks.Spec.Path != "" {
		return filepath.Join(repoRoot, ks.Spec.Path)
	}
	return ""
}
