package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/banschikovde/fluxview/internal/flux"
	"github.com/banschikovde/fluxview/internal/git"
	"github.com/banschikovde/fluxview/internal/kustomize"
	"github.com/banschikovde/fluxview/internal/validate"
)

// ValidateFlags holds flags for the validate command.
type ValidateFlags struct {
	Path      string
	Namespace string
	SchemaDir string
	SkipCRDs  bool
}

func newValidateCmd() *cobra.Command {
	flags := &ValidateFlags{}

	cmd := &cobra.Command{
		Use:   "validate <resource> [name] [flags]",
		Short: "Validate Flux resources against CRD schemas",
		Long: `Validate built Flux resources against bundled CRD schemas.

Resources without a matching CRD schema are silently skipped.
Schemas are loaded from --schema-dir (default: ./crds/).

Examples:
  fluxview validate ks --path clusters/prod/
  fluxview validate ks --path clusters/prod/ --schema-dir /crds`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runValidate(cmd.Context(), args, flags)
		},
	}

	cmd.Flags().StringVarP(&flags.Path, "path", "p", "", "Path to the cluster directory in the repository")
	cmd.Flags().StringVarP(&flags.Namespace, "namespace", "n", "", "Filter output resources by namespace")
	cmd.Flags().StringVar(&flags.SchemaDir, "schema-dir", "", "Directory with CRD schema files (default: ./crds/)")
	cmd.Flags().BoolVar(&flags.SkipCRDs, "skip-crds", false, "Skip CustomResourceDefinition resources")

	return cmd
}

func runValidate(ctx context.Context, args []string, flags *ValidateFlags) error {
	resourceType := args[0]
	var name string
	if len(args) > 1 {
		name = args[1]
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
		return NewExitError(fmt.Errorf("finding git repo root: %w", err), ExitCodeError)
	}

	// Resolve schema directory.
	schemaDir := flags.SchemaDir
	if schemaDir == "" {
		schemaDir = defaultSchemaDir()
	}

	validator := validate.New(schemaDir)

	fmt.Fprintf(os.Stderr, "Loaded %d CRD schemas from %s\n", validator.SchemaCount(), schemaDir)

	switch resourceType {
	case "ks", "kustomization":
		return runValidateKS(ctx, absClusterPath, repoRoot, name, flags.Namespace, flags.SkipCRDs, validator)
	default:
		return NewExitError(fmt.Errorf("unsupported resource type %q (currently only 'ks' is supported)", resourceType), ExitCodeError)
	}
}

func runValidateKS(ctx context.Context, clusterPath, repoRoot, name, namespace string, skipCRDs bool, validator *validate.Validator) error {
	parser := flux.NewParser(clusterPath)
	kustomizations, err := parser.ParseKustomizations(ctx)
	if err != nil {
		return NewExitError(fmt.Errorf("parsing Kustomization resources: %w", err), ExitCodeError)
	}

	configMaps, _ := resolveConfigMaps(ctx, clusterPath)

	if name != "" {
		kustomizations = filterKustomizations(kustomizations, name)
		if len(kustomizations) == 0 {
			return NewExitError(fmt.Errorf("Kustomization %q not found", name), ExitCodeError)
		}
	}

	builder := kustomize.NewBuilder()
	output, err := buildKSContent(ctx, builder, kustomizations, repoRoot, clusterPath, configMaps, false)
	if err != nil {
		return NewExitError(err, ExitCodeError)
	}

	if output == nil {
		fmt.Fprintln(os.Stderr, "No resources to validate.")
		return nil
	}

	if skipCRDs {
		output = filterCRDDocs(output)
	}

	if namespace != "" {
		output = filterByNamespace(output, namespace)
		if len(output) == 0 {
			fmt.Fprintf(os.Stderr, "No resources found in namespace %q\n", namespace)
			return nil
		}
	}

	results := validator.Validate(output)
	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "All resources valid.")
		return nil
	}

	for _, r := range results {
		fmt.Fprintf(os.Stderr, "✗ %s\n", r.Resource)
		for _, e := range r.Errors {
			fmt.Fprintf(os.Stderr, "  %s\n", e)
		}
	}

	return NewExitError(fmt.Errorf("%d resource(s) failed validation", len(results)), ExitValidationFailed)
}

// defaultSchemaDir returns the default CRD schema directory.
// Checks common locations: /crds/ (Docker), ./crds/ (local).
func defaultSchemaDir() string {
	for _, dir := range []string{"/crds", "crds"} {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
	}
	return "crds"
}
