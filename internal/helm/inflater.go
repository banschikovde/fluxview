// Package helm provides orchestration of Helm chart rendering via the Helm Go SDK.
package helm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/registry"
	"helm.sh/helm/v4/pkg/release"
	"helm.sh/helm/v4/pkg/storage"
	"helm.sh/helm/v4/pkg/storage/driver"

	fluxtypes "github.com/banschikovde/fluxview/internal/flux"
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
	cacheDir := filepath.Join(os.TempDir(), "fluxview-helm-cache")
	settings.RepositoryCache = filepath.Join(cacheDir, "repository")
	settings.RepositoryConfig = filepath.Join(cacheDir, "repositories.yaml")
	_ = os.MkdirAll(settings.RepositoryCache, 0755)

	return &Inflater{settings: settings}, nil
}

// InflateHelmRelease inflates a Flux HelmRelease resource using the Helm Go SDK.
// It locates/downloads the chart from the given repo URL and renders templates
// equivalent to: helm template <name> <chart> --repo <url> --version <ver> --namespace <ns> --include-crds
func (in *Inflater) InflateHelmRelease(ctx context.Context, hr fluxtypes.HelmRelease, repoURL string, username, password string, configMaps []fluxtypes.ConfigMap, secrets []fluxtypes.Secret) ([]byte, error) {
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

	// Chart resolution: OCI repos need special handling.
	// See helm/helm#10191: setting RepoURL for OCI causes index.yaml fetch failure.
	var chartRef string
	switch {
	case strings.HasPrefix(chartName, "oci://"):
		// OCIRepository pattern: chartName is the full OCI reference
		// (URL + optional @digest). Use directly, don't append anything.
		chartRef = chartName
		install.ChartPathOptions.Version = hr.Spec.Chart.Spec.Version

		// Create registry client with necessary options for OCI charts
		opts := []registry.ClientOption{
			registry.ClientOptEnableCache(true),
		}
		if username != "" && password != "" {
			opts = append(opts, registry.ClientOptBasicAuth(username, password))
		}

		registryClient, err := registry.NewClient(opts...)
		if err != nil {
			return nil, fmt.Errorf("creating registry client: %w", err)
		}
		actionConfig.RegistryClient = registryClient
		install.SetRegistryClient(registryClient)
	case strings.HasPrefix(repoURL, "oci://"):
		// HelmRepository type=oci: append chart name to repo URL.
		chartRef = strings.TrimSuffix(repoURL, "/") + "/" + chartName
		install.ChartPathOptions.Version = hr.Spec.Chart.Spec.Version

		// Create registry client with necessary options for OCI charts
		opts := []registry.ClientOption{
			registry.ClientOptEnableCache(true),
		}
		if username != "" && password != "" {
			opts = append(opts, registry.ClientOptBasicAuth(username, password))
		}

		registryClient, err := registry.NewClient(opts...)
		if err != nil {
			return nil, fmt.Errorf("creating registry client: %w", err)
		}
		actionConfig.RegistryClient = registryClient
		install.SetRegistryClient(registryClient)
	default:
		// Traditional HTTP HelmRepository.
		chartRef = chartName
		install.ChartPathOptions.RepoURL = repoURL
		install.ChartPathOptions.Version = hr.Spec.Chart.Spec.Version
		// Set credentials for HTTP repository if provided.
		if username != "" && password != "" {
			install.ChartPathOptions.Username = username
			install.ChartPathOptions.Password = password
		}
	}

	// Locate the chart (downloads from repo if necessary).
	chartPath, err := install.ChartPathOptions.LocateChart(chartRef, in.settings)
	if err != nil {
		return nil, fmt.Errorf("locating chart %s: %w", chartRef, err)
	}

	// Load the chart.
	chartObj, err := loader.Load(chartPath)
	if err != nil {
		return nil, fmt.Errorf("loading chart from %s: %w", chartPath, err)
	}

	// Start with valuesFrom (ConfigMaps and Secrets) as base values.
	values := fluxtypes.ResolveValuesFrom(hr, configMaps, secrets)
	if values == nil {
		values = make(map[string]interface{})
	}

	// Merge inline values from HelmRelease spec (they have highest priority).
	for k, v := range hr.Spec.Values {
		values[k] = v
	}

	// Run template rendering.
	rel, err := install.RunWithContext(ctx, chartObj, values)
	if err != nil {
		return nil, fmt.Errorf("rendering chart %s: %w", chartRef, err)
	}

	// Extract manifest via accessor (Helm v4 returns Releaser interface).
	accessor, err := release.NewAccessor(rel)
	if err != nil {
		return nil, fmt.Errorf("accessing release manifest: %w", err)
	}

	manifest := accessor.Manifest()

	// Convert JSON-in-YAML to proper YAML format
	// Helm v4 sometimes renders large objects (especially CRDs) as JSON in YAML
	converted, err := convertJSONInYAMLToYAML([]byte(manifest))
	if err != nil {
		// If conversion fails, return original manifest
		return []byte(manifest), nil
	}

	return converted, nil
}

// FindHelmRepoURL finds the URL for a HelmRepository referenced by a HelmRelease.
// For OCI repositories (type: oci), the URL is prefixed with oci:// as required
// by the Helm SDK.
func FindHelmRepoURL(repos []fluxtypes.HelmRepository, name, namespace string, secrets []fluxtypes.Secret) (string, string, string, error) {
	for _, repo := range repos {
		if repo.Metadata.Name == name && repo.Metadata.Namespace == namespace {
			url := repo.Spec.URL
			if repo.Spec.Type == "oci" && !strings.HasPrefix(url, "oci://") {
				url = strings.TrimPrefix(url, "https://")
				url = strings.TrimPrefix(url, "http://")
				url = "oci://" + url
			}

			username := ""
			password := ""
			if repo.Spec.SecretRef != nil {
				secretNS := namespace
				for _, secret := range secrets {
					if secret.Metadata.Name == repo.Spec.SecretRef.Name && secret.Metadata.Namespace == secretNS {
						username = secret.GetSecretValue("username")
						password = secret.GetSecretValue("password")
						break
					}
				}
			}

			return url, username, password, nil
		}
	}
	return "", "", "", fmt.Errorf("HelmRepository %s/%s not found", namespace, name)
}

// convertJSONInYAMLToYAML converts JSON-in-YAML format (e.g., metadata: {...})
// to proper YAML format (e.g., metadata:\n  annotations: ...).
// Helm v4 sometimes renders large objects like CRDs as JSON in YAML.
// Handles multi-document YAML by decoding each document separately.
// Removes nil map values (e.g. annotations: null) produced by Helm templates
// with empty optional fields.
func convertJSONInYAMLToYAML(manifest []byte) ([]byte, error) {
	var docs []string

	decoder := yaml.NewDecoder(bytes.NewReader(manifest))
	for {
		var doc interface{}
		if err := decoder.Decode(&doc); err != nil {
			if err == io.EOF {
				break
			}
			continue
		}
		if doc == nil {
			continue
		}
		doc = removeNilValues(doc)
		marshaled, err := yaml.Marshal(doc)
		if err != nil {
			continue
		}
		docs = append(docs, strings.TrimRight(string(marshaled), "\n"))
	}

	if len(docs) == 0 {
		return nil, nil
	}
	return []byte(strings.Join(docs, "\n---\n")), nil
}

// removeNilValues recursively removes map entries with nil values.
func removeNilValues(in interface{}) interface{} {
	switch v := in.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{})
		for k, val := range v {
			if val == nil {
				continue
			}
			result[k] = removeNilValues(val)
		}
		return result
	case []interface{}:
		for i, val := range v {
			v[i] = removeNilValues(val)
		}
		return v
	default:
		return in
	}
}
