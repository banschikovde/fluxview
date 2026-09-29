package inventory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// parseCR is a test helper turning a YAML document into the map the rule
// engine evaluates (same conversion the collector performs).
func parseCR(t *testing.T, doc string) map[string]interface{} {
	t.Helper()
	var raw map[string]interface{}
	if err := yaml.Unmarshal([]byte(doc), &raw); err != nil {
		t.Fatalf("parsing CR: %v", err)
	}
	return raw
}

func TestRuleExtract_FieldsInOrder(t *testing.T) {
	rs, err := NewRuleSet([]Rule{
		{Group: "operator.victoriametrics.com", Kind: "VMCluster", Software: "VictoriaMetrics cluster",
			Version: []VersionField{{JSONPath: ".spec.clusterVersion"}, {JSONPath: ".spec.vmstorage.image.tag"}}},
	})
	if err != nil {
		t.Fatalf("NewRuleSet: %v", err)
	}
	rule, ok := rs.Lookup("operator.victoriametrics.com", "VMCluster")
	if !ok {
		t.Fatal("rule not found")
	}

	cr := parseCR(t, `apiVersion: operator.victoriametrics.com/v1beta1
kind: VMCluster
metadata: {name: vmc, namespace: monitoring}
spec:
  clusterVersion: v1.146.0-cluster
  vmstorage:
    image: {tag: v1.146.0-cluster}
`)
	if v, ok := rule.Extract(cr); !ok || v != "v1.146.0-cluster" {
		t.Errorf("Extract = %q, %v; want v1.146.0-cluster", v, ok)
	}

	// clusterVersion absent → falls through to the image tag.
	cr = parseCR(t, `apiVersion: operator.victoriametrics.com/v1beta1
kind: VMCluster
metadata: {name: vmc}
spec:
  vmstorage:
    image: {tag: v1.146.0}
`)
	if v, ok := rule.Extract(cr); !ok || v != "v1.146.0" {
		t.Errorf("Extract fallback = %q, %v; want v1.146.0", v, ok)
	}

	// Nothing set → not ok (caller renders "default").
	cr = parseCR(t, `apiVersion: operator.victoriametrics.com/v1beta1
kind: VMCluster
metadata: {name: vmc}
spec: {}
`)
	if v, ok := rule.Extract(cr); ok || v != "" {
		t.Errorf("Extract empty = %q, %v; want empty/false", v, ok)
	}
}

func TestRuleExtract_RegexAndNumbers(t *testing.T) {
	rs, err := NewRuleSet([]Rule{
		{Group: "vault.banzaicloud.com", Kind: "Vault", Software: "Vault",
			Version: []VersionField{{JSONPath: ".spec.image", Regex: `:(?P<version>[^@]+)`}}},
		{Group: "example.com", Kind: "Widget", Software: "Widget",
			Version: []VersionField{{JSONPath: ".spec.version"}}},
	})
	if err != nil {
		t.Fatalf("NewRuleSet: %v", err)
	}

	vault, _ := rs.Lookup("vault.banzaicloud.com", "Vault")
	cr := parseCR(t, `apiVersion: vault.banzaicloud.com/v1alpha1
kind: Vault
metadata: {name: vault, namespace: vault}
spec:
  image: hashicorp/vault:2.1.1
`)
	if v, ok := vault.Extract(cr); !ok || v != "2.1.1" {
		t.Errorf("Vault Extract = %q, %v; want 2.1.1", v, ok)
	}

	// Digest suffix must not leak into the version.
	cr = parseCR(t, `apiVersion: vault.banzaicloud.com/v1alpha1
kind: Vault
metadata: {name: vault}
spec:
  image: hashicorp/vault:2.1.1@sha256:abcdef
`)
	if v, ok := vault.Extract(cr); !ok || v != "2.1.1" {
		t.Errorf("Vault digest Extract = %q, %v; want 2.1.1", v, ok)
	}

	// No tag at all → regex misses → next field/default.
	cr = parseCR(t, `apiVersion: vault.banzaicloud.com/v1alpha1
kind: Vault
metadata: {name: vault}
spec:
  image: hashicorp/vault
`)
	if v, ok := vault.Extract(cr); ok || v != "" {
		t.Errorf("Vault no-tag Extract = %q, %v; want empty/false", v, ok)
	}

	// Numeric version fields stringify without decoration.
	widget, _ := rs.Lookup("example.com", "Widget")
	cr = parseCR(t, `apiVersion: example.com/v1
kind: Widget
metadata: {name: w1}
spec:
  version: 16
`)
	if v, ok := widget.Extract(cr); !ok || v != "16" {
		t.Errorf("numeric Extract = %q, %v; want 16", v, ok)
	}
}

// TestExampleRulesFile_CompileAndExtract keeps docs/inventory-rules-example.yaml
// honest: it must parse and extract versions for the operators it documents.
// The binary ships no built-in rules — the example is the copyable starting
// point users own themselves.
func TestExampleRulesFile_CompileAndExtract(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "inventory-rules-example.yaml"))
	if err != nil {
		t.Fatalf("reading example rules: %v", err)
	}
	rules, err := ParseRuleFile(data)
	if err != nil {
		t.Fatalf("parsing example rules: %v", err)
	}
	rs, err := NewRuleSet(rules)
	if err != nil {
		t.Fatalf("NewRuleSet(example): %v", err)
	}
	if rs.Len() != len(rules) {
		t.Errorf("rule count = %d, want %d", rs.Len(), len(rules))
	}

	cases := []struct {
		group, kind, cr, want string
	}{
		{"operator.victoriametrics.com", "VMAuth",
			`apiVersion: operator.victoriametrics.com/v1beta1
kind: VMAuth
metadata: {name: vmcluster}
spec: {image: {tag: v1.146.0}}`, "v1.146.0"},
		{"operator.victoriametrics.com", "VMAlertmanager",
			`apiVersion: operator.victoriametrics.com/v1
kind: VMAlertmanager
metadata: {name: vmalertmanager}
spec: {image: {tag: v0.34.1}}`, "v0.34.1"},
		{"operator.victoriametrics.com", "VLAgent",
			`apiVersion: operator.victoriametrics.com/v1
kind: VLAgent
metadata: {name: vlogs}
spec: {image: {tag: v1.52.0}}`, "v1.52.0"},
		{"operator.victoriametrics.com", "VLSingle",
			`apiVersion: operator.victoriametrics.com/v1
kind: VLSingle
metadata: {name: standby}
spec: {image: {tag: v1.52.0}}`, "v1.52.0"},
		{"kafka.strimzi.io", "Kafka",
			`apiVersion: kafka.strimzi.io/v1beta2
kind: Kafka
metadata: {name: strimzi}
spec: {kafka: {version: 3.8.0}}`, "3.8.0"},
		{"vault.banzaicloud.com", "Vault",
			`apiVersion: vault.banzaicloud.com/v1alpha1
kind: Vault
metadata: {name: vault}
spec: {image: hashicorp/vault:2.1.1}`, "2.1.1"},
		{"grafana.integreatly.org", "Grafana",
			`apiVersion: grafana.integreatly.org/v1beta1
kind: Grafana
metadata: {name: grafana}
spec: {version: 13.2.1-ubuntu}`, "13.2.1-ubuntu"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			rule, ok := rs.Lookup(tc.group, tc.kind)
			if !ok {
				t.Fatalf("example rule %s/%s missing", tc.group, tc.kind)
			}
			v, ok := rule.Extract(parseCR(t, tc.cr))
			if !ok || v != tc.want {
				t.Errorf("Extract = %q, %v; want %q", v, ok, tc.want)
			}
		})
	}
}

// TestLaterRuleOverridesEarlier pins the override semantics a user relies on
// when layering rules: for one group/kind, the later entry wins.
func TestLaterRuleOverridesEarlier(t *testing.T) {
	all := []Rule{
		{Group: "operator.victoriametrics.com", Kind: "VMCluster", Software: "VM cluster (stock)",
			Version: []VersionField{{JSONPath: ".spec.clusterVersion"}}},
		{Group: "operator.victoriametrics.com", Kind: "VMCluster", Software: "VM cluster (custom)",
			Version: []VersionField{{JSONPath: ".spec.retentionPeriod"}}},
	}
	rs, err := NewRuleSet(all)
	if err != nil {
		t.Fatalf("NewRuleSet: %v", err)
	}
	rule, ok := rs.Lookup("operator.victoriametrics.com", "VMCluster")
	if !ok {
		t.Fatal("rule missing")
	}
	if rule.Software != "VM cluster (custom)" {
		t.Errorf("later rule must override the earlier one: Software = %q", rule.Software)
	}

	cr := parseCR(t, `apiVersion: operator.victoriametrics.com/v1beta1
kind: VMCluster
metadata: {name: vmc}
spec: {retentionPeriod: 30d, clusterVersion: v1.146.0}
`)
	if v, ok := rule.Extract(cr); !ok || v != "30d" {
		t.Errorf("overridden Extract = %q, %v; want 30d", v, ok)
	}
}

func TestExtractImage(t *testing.T) {
	rs, err := NewRuleSet([]Rule{
		{Group: "vault.banzaicloud.com", Kind: "Vault", Software: "Vault",
			Version: []VersionField{{JSONPath: ".spec.image", Regex: `:(?P<version>[^@]+)`}},
			Image:   []VersionField{{JSONPath: ".spec.image"}}},
		{Group: "operator.victoriametrics.com", Kind: "VMAuth", Software: "VMAuth",
			Version: []VersionField{{JSONPath: ".spec.image.tag"}},
			Image:   []VersionField{{JSONPath: "{.spec.image.repository}:{.spec.image.tag}"}}},
		{Group: "operator.victoriametrics.com", Kind: "VMCluster", Software: "VictoriaMetrics cluster",
			Version: []VersionField{{JSONPath: ".spec.clusterVersion"}}},
	})
	if err != nil {
		t.Fatalf("NewRuleSet: %v", err)
	}

	vault, _ := rs.Lookup("vault.banzaicloud.com", "Vault")
	img, ok := vault.ExtractImage(parseCR(t, `spec: {image: hashicorp/vault:2.1.1}`))
	if !ok || img != "hashicorp/vault:2.1.1" {
		t.Errorf("Vault ExtractImage = %q, %v; want hashicorp/vault:2.1.1", img, ok)
	}

	// Composed reference: repository and tag live in separate fields.
	vmauth, _ := rs.Lookup("operator.victoriametrics.com", "VMAuth")
	img, ok = vmauth.ExtractImage(parseCR(t, `spec:
  image:
    repository: quay.io/victoriametrics/vmauth
    tag: v1.152.0
`))
	if !ok || img != "quay.io/victoriametrics/vmauth:v1.152.0" {
		t.Errorf("VMAuth ExtractImage = %q, %v; want quay.io/victoriametrics/vmauth:v1.152.0", img, ok)
	}

	// Missing repository → the composed field renders a half reference
	// (":v1.152.0"), which is rejected — a tag alone is not an image.
	img, ok = vmauth.ExtractImage(parseCR(t, `spec: {image: {tag: v1.152.0}}`))
	if ok {
		t.Errorf("half-composed reference must be rejected, got %q", img)
	}

	// Rule without image fields → not ok.
	vmcluster, _ := rs.Lookup("operator.victoriametrics.com", "VMCluster")
	if _, ok := vmcluster.ExtractImage(parseCR(t, `spec: {clusterVersion: v1.146.0-cluster}`)); ok {
		t.Errorf("rule without image fields must not report an image")
	}
}

func TestRuleFieldTemplateForms(t *testing.T) {
	cr := `spec:
  image:
    repository: quay.io/victoriametrics/vmauth
    tag: v1.152.0
`
	// A bare path (no braces) and its braced form are equivalent.
	rs, err := NewRuleSet([]Rule{
		{Group: "g", Kind: "K", Software: "s",
			Version: []VersionField{{JSONPath: "{.spec.image.tag}"}}},
		{Group: "g2", Kind: "K", Software: "s",
			Version: []VersionField{{JSONPath: ".spec.image.tag"}}},
	})
	if err != nil {
		t.Fatalf("NewRuleSet: %v", err)
	}
	for _, g := range []string{"g", "g2"} {
		rule, _ := rs.Lookup(g, "K")
		if v, ok := rule.Extract(parseCR(t, cr)); !ok || v != "v1.152.0" {
			t.Errorf("%s: bare and braced forms must be equivalent, got %q, %v", g, v, ok)
		}
	}
}

func TestParseRuleFile(t *testing.T) {
	data := []byte(`rules:
  - group: example.com
    kind: Widget
    software: Widget
    version:
      - jsonpath: .spec.version
      - jsonpath: .spec.image
        regex: ':(?P<version>[^@]+)'
`)
	rules, err := ParseRuleFile(data)
	if err != nil {
		t.Fatalf("ParseRuleFile: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("got %d rules, want 1", len(rules))
	}
	if rules[0].Version[1].Regex == "" {
		t.Error("regex field not parsed")
	}

	if _, err := ParseRuleFile([]byte("rules: [not, a, map]")); err == nil {
		t.Error("malformed rules file must error")
	}
	if _, err := ParseRuleFile([]byte("- a\n- b\n")); err == nil {
		t.Error("non-mapping rules file must error")
	}
}

func TestNewRuleSet_Validation(t *testing.T) {
	cases := []struct {
		name  string
		rules []Rule
	}{
		{"missing group", []Rule{{Kind: "K", Software: "s", Version: []VersionField{{JSONPath: ".a"}}}}},
		{"missing kind", []Rule{{Group: "g", Software: "s", Version: []VersionField{{JSONPath: ".a"}}}}},
		{"missing software", []Rule{{Group: "g", Kind: "K", Version: []VersionField{{JSONPath: ".a"}}}}},
		{"no version fields", []Rule{{Group: "g", Kind: "K", Software: "s"}}},
		{"empty jsonpath", []Rule{{Group: "g", Kind: "K", Software: "s", Version: []VersionField{{}}}}},
		{"bad jsonpath", []Rule{{Group: "g", Kind: "K", Software: "s", Version: []VersionField{{JSONPath: "spec[["}}}}},
		{"bad regex", []Rule{{Group: "g", Kind: "K", Software: "s", Version: []VersionField{{JSONPath: ".a", Regex: "("}}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRuleSet(tc.rules); err == nil {
				t.Error("invalid rule must be rejected")
			}
		})
	}
}

func TestIsCRCandidate(t *testing.T) {
	cases := []struct {
		apiVersion string
		want       bool
	}{
		{"v1", false},
		{"apps/v1", false},
		{"batch/v1", false},
		{"apiextensions.k8s.io/v1", false},
		{"helm.toolkit.fluxcd.io/v2", false},
		{"kustomize.toolkit.fluxcd.io/v1", false},
		{"source.toolkit.fluxcd.io/v1", false},
		{"operator.victoriametrics.com/v1beta1", true},
		{"postgresql.cnpg.io/v1", true},
		{"kafka.strimzi.io/v1beta2", true},
		{"", false},
	}
	for _, tc := range cases {
		if got := IsCRCandidate(tc.apiVersion); got != tc.want {
			t.Errorf("IsCRCandidate(%q) = %v, want %v", tc.apiVersion, got, tc.want)
		}
	}
}

func TestExtract_UniversalVersionRegexNoColon(t *testing.T) {
	// A regex whose "version" group matches empty must not win over a later
	// non-empty field.
	rs, err := NewRuleSet([]Rule{
		{Group: "g", Kind: "K", Software: "s",
			Version: []VersionField{
				{JSONPath: ".spec.image", Regex: `:(?P<version>[^@]+)`},
				{JSONPath: ".spec.fallback"},
			}},
	})
	if err != nil {
		t.Fatalf("NewRuleSet: %v", err)
	}
	rule, _ := rs.Lookup("g", "K")
	cr := parseCR(t, `spec:
  image: 000000.dkr.ecr.example/library/redis
  fallback: 7.2.4
`)
	if v, ok := rule.Extract(cr); !ok || !strings.HasPrefix(v, "7.") {
		t.Errorf("Extract = %q, %v; want the fallback field", v, ok)
	}
}
