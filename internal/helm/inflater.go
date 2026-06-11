// Package helm provides orchestration of Helm chart rendering via the Helm Go SDK.
package helm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/release"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"

	"gopkg.in/yaml.v3"

	fluxtypes "github.com/banschikovde/flux-diff/internal/flux"
)

// Inflater renders Helm charts via the Helm Go SDK.
type Inflater struct {
	settings *cli.EnvSettings
}

// NewInflater creates a new Helm Inflater using the Go SDK.
// No external helm binary is required.
func NewInflater() (*Inflater, error) {
	settings := cli.New()

	// Use a dedicated cache directory for helm repos/charts.
	cacheDir := filepath.Join(os.TempDir(), "flux-diff-helm-cache")
	settings.RepositoryCache = filepath.Join(cacheDir, "repository")
	settings.RepositoryConfig = filepath.Join(cacheDir, "repositories.yaml")
	_ = os.MkdirAll(settings.RepositoryCache, 0755)

	return &Inflater{settings: settings}, nil
}

// InflateHelmRelease inflates a Flux HelmRelease resource using the Helm Go SDK.
// It locates/downloads the chart from the given repo URL and renders templates
// equivalent to: helm template <name> <chart> --repo <url> --version <ver> --namespace <ns> --include-crds
func (in *Inflater) InflateHelmRelease(ctx context.Context, hr fluxtypes.HelmRelease, repoURL string) ([]byte, error) {
	chartName := hr.Spec.Chart.Spec.Chart

	// Set up the action configuration for template rendering (no k8s cluster needed).
	actionConfig := &action.Configuration{
		Releases:     storage.Init(driver.NewMemory()),
		Capabilities: common.DefaultCapabilities,
	}

	install := action.NewInstall(actionConfig)
	install.DryRunStrategy = action.DryRunClient
	install.IncludeCRDs = true
	install.ReleaseName = hr.Metadata.Name

	// Determine target namespace.
	namespace := hr.Metadata.Namespace
	if hr.Spec.TargetNamespace != "" {
		namespace = hr.Spec.TargetNamespace
	}
	install.Namespace = namespace

	// Chart resolution options.
	install.ChartPathOptions.RepoURL = repoURL
	install.ChartPathOptions.Version = hr.Spec.Chart.Spec.Version

	// Locate the chart (downloads from repo if necessary).
	chartPath, err := install.ChartPathOptions.LocateChart(chartName, in.settings)
	if err != nil {
		return nil, fmt.Errorf("locating chart %s: %w", chartName, err)
	}

	// Load the chart.
	chartObj, err := loader.Load(chartPath)
	if err != nil {
		return nil, fmt.Errorf("loading chart from %s: %w", chartPath, err)
	}

	// Get values from HelmRelease spec.
	values := hr.Spec.Values
	if values == nil {
		values = make(map[string]interface{})
	}

	// Run template rendering.
	rel, err := install.RunWithContext(ctx, chartObj, values)
	if err != nil {
		return nil, fmt.Errorf("rendering chart %s: %w", chartName, err)
	}

	// Extract manifest via accessor (Helm v4 returns Releaser interface).
	accessor, err := release.NewAccessor(rel)
	if err != nil {
		return nil, fmt.Errorf("accessing release manifest: %w", err)
	}

	return []byte(accessor.Manifest()), nil
}

// FindHelmRepoURL finds the URL for a HelmRepository referenced by a HelmRelease.
func FindHelmRepoURL(repos []fluxtypes.HelmRepository, name, namespace string) (string, error) {
	for _, repo := range repos {
		if repo.Metadata.Name == name && repo.Metadata.Namespace == namespace {
			return repo.Spec.URL, nil
		}
	}
	return "", fmt.Errorf("HelmRepository %s/%s not found", namespace, name)
}

// ExtractYAMLDocuments splits multi-document YAML into individual byte slices.
func ExtractYAMLDocuments(data []byte) [][]byte {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var docs [][]byte

	for {
		var node yaml.Node
		if err := decoder.Decode(&node); err != nil {
			break
		}
		var buf bytes.Buffer
		encoder := yaml.NewEncoder(&buf)
		encoder.SetIndent(2)
		if err := encoder.Encode(&node); err != nil {
			break
		}
		encoder.Close()

		content := strings.TrimSpace(buf.String())
		if content != "" {
			docs = append(docs, []byte(content))
		}
	}

	return docs
}
