package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/banschikovde/fluxview/internal/validate"
)

func rendererFixtures() []validate.Result {
	return []validate.Result{
		{Kind: "Widget", Name: "good", Namespace: "default", Status: validate.StatusValid},
		{Kind: "Widget", Name: "bad", Status: validate.StatusInvalid, Errors: []string{"spec.color: color is required"}},
		{Kind: "Gadget", Name: "noschema", Status: validate.StatusSkipped},
		{Name: "broken", Status: validate.StatusError, Errors: []string{"missing 'kind' key"}},
	}
}

func TestWriteValidationText(t *testing.T) {
	var buf bytes.Buffer
	writeValidationText(&buf, []validate.Result{
		{Kind: "Widget", Name: "bad", Namespace: "default", Status: validate.StatusInvalid, Errors: []string{"spec.color: color is required"}},
		{Name: "broken", Status: validate.StatusError, Errors: []string{"missing 'kind' key"}},
	})

	want := "✗ Widget default/bad\n  spec.color: color is required\n✗ malformed resource\n  missing 'kind' key\n"
	if buf.String() != want {
		t.Errorf("text output = %q, want %q", buf.String(), want)
	}
}

func TestWriteValidationJSON(t *testing.T) {
	var buf bytes.Buffer
	if err := writeValidationJSON(&buf, rendererFixtures(), nil); err != nil {
		t.Fatal(err)
	}

	var report jsonValidationReport
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}

	if report.Summary != (jsonValidationSummary{Valid: 1, Skipped: 1, Invalid: 1, Errors: 1}) {
		t.Errorf("summary = %+v, want valid/skipped/invalid/errors = 1/1/1/1", report.Summary)
	}
	if len(report.Resources) != 4 {
		t.Fatalf("resources = %d, want 4", len(report.Resources))
	}

	// The malformed resource gets a synthetic kind for grouping.
	if report.Resources[3].Kind != "malformed" || report.Resources[3].Status != "error" {
		t.Errorf("malformed resource = %+v, want kind=malformed status=error", report.Resources[3])
	}
	if report.Resources[1].Errors[0] != "spec.color: color is required" {
		t.Errorf("invalid resource errors = %v", report.Resources[1].Errors)
	}
	// A fully checked run carries no "unchecked" key at all.
	if report.Unchecked != nil {
		t.Errorf("unchecked = %+v, want none", report.Unchecked)
	}
	if strings.Contains(buf.String(), "unchecked") {
		t.Errorf("a fully checked run must not emit the unchecked field:\n%s", buf.String())
	}
}

func TestWriteValidationJSON_Unchecked(t *testing.T) {
	unchecked := []uncheckedGroup{
		{kustomizations: []string{"flux-system/envoy-crds", "flux-system/envoy-extra"}, source: "OCIRepository flux-system/envoy", cause: "source kind OCIRepository is not fetched"},
		{kustomizations: []string{"kyverno/kyverno-crds"}, source: "GitRepository kyverno/kyverno", cause: "disabled by --no-git-source-fetch"},
	}
	var buf bytes.Buffer
	if err := writeValidationJSON(&buf, rendererFixtures(), unchecked); err != nil {
		t.Fatal(err)
	}

	var report jsonValidationReport
	if err := json.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if len(report.Unchecked) != 3 {
		t.Fatalf("unchecked = %d entries, want 3:\n%s", len(report.Unchecked), buf.String())
	}
	want := jsonUncheckedKustomization{
		Kustomization: "flux-system/envoy-crds",
		Source:        "OCIRepository flux-system/envoy",
		Cause:         "source kind OCIRepository is not fetched",
	}
	if report.Unchecked[0] != want {
		t.Errorf("first unchecked entry = %+v, want %+v", report.Unchecked[0], want)
	}
	if report.Unchecked[1].Kustomization != "flux-system/envoy-extra" || report.Unchecked[2].Kustomization != "kyverno/kyverno-crds" {
		t.Errorf("group order not preserved: %+v", report.Unchecked)
	}
	// The summary stays about resources.
	if report.Summary != (jsonValidationSummary{Valid: 1, Skipped: 1, Invalid: 1, Errors: 1}) {
		t.Errorf("summary = %+v — unchecked entries must not touch resource counts", report.Summary)
	}
}

func TestWriteValidationJSON_EmptyRun(t *testing.T) {
	var buf bytes.Buffer
	if err := writeValidationJSON(&buf, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"resources": []`) {
		t.Errorf("empty run should render an empty list, got:\n%s", buf.String())
	}
}

type parsedJUnit struct {
	XMLName  xml.Name `xml:"testsuites"`
	Tests    int      `xml:"tests,attr"`
	Failures int      `xml:"failures,attr"`
	Errors   int      `xml:"errors,attr"`
	Skipped  int      `xml:"skipped,attr"`
	Suites   []struct {
		Cases []struct {
			Name    string `xml:"name,attr"`
			Failure *struct {
				Message string `xml:"message,attr"`
				Text    string `xml:",chardata"`
			} `xml:"failure"`
			Error *struct {
				Message string `xml:"message,attr"`
			} `xml:"error"`
			Skipped *struct{} `xml:"skipped"`
		} `xml:"testcase"`
	} `xml:"testsuite"`
}

func TestWriteValidationJUnit(t *testing.T) {
	var buf bytes.Buffer
	if err := writeValidationJUnit(&buf, rendererFixtures(), nil); err != nil {
		t.Fatal(err)
	}

	if !strings.HasPrefix(buf.String(), "<?xml version=\"1.0\" encoding=\"UTF-8\"?>") {
		t.Errorf("junit output should start with the XML declaration, got:\n%s", buf.String())
	}

	var report parsedJUnit
	if err := xml.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("output is not valid XML: %v\n%s", err, buf.String())
	}

	if report.Tests != 4 || report.Failures != 1 || report.Errors != 1 || report.Skipped != 1 {
		t.Errorf("counts = tests %d, failures %d, errors %d, skipped %d; want 4/1/1/1",
			report.Tests, report.Failures, report.Errors, report.Skipped)
	}

	cases := report.Suites[0].Cases
	if len(cases) != 4 {
		t.Fatalf("testcases = %d, want 4", len(cases))
	}
	if cases[0].Name != "Widget default/good" || cases[0].Failure != nil {
		t.Errorf("valid resource should be a plain testcase, got %+v", cases[0])
	}
	if cases[1].Failure == nil || !strings.Contains(cases[1].Failure.Text, "spec.color") {
		t.Errorf("invalid resource should carry a <failure> with the error, got %+v", cases[1])
	}
	if cases[2].Skipped == nil {
		t.Errorf("skipped resource should carry <skipped>, got %+v", cases[2])
	}
	if cases[3].Error == nil {
		t.Errorf("error resource should carry <error>, got %+v", cases[3])
	}
}

// TestRunValidate_OutputJSON wires --output json end to end: the machine
// report goes to stdout (stderr keeps the human chatter) and the exit code
// still reflects validation failure.
func TestRunValidate_OutputJSON(t *testing.T) {
	repoRoot := t.TempDir()
	gitInit(t, repoRoot)

	clusterDir := filepath.Join(repoRoot, "cluster")
	writeHelper(t, clusterDir, "ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: app
  namespace: flux-system
spec:
  interval: 5m
  path: ./app
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
`)

	// One valid and one invalid Widget (missing spec.color).
	appDir := filepath.Join(repoRoot, "app")
	writeHelper(t, appDir, "kustomization.yaml", `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - widgets.yaml
`)
	writeHelper(t, appDir, "widgets.yaml", `apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: good-widget
spec:
  size: large
  color: red
---
apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: bad-widget
spec:
  size: large
`)

	schemaDir := filepath.Join(repoRoot, "schemas")
	if err := os.MkdirAll(schemaDir, 0755); err != nil {
		t.Fatalf("mkdir schema dir: %v", err)
	}
	writeHelper(t, schemaDir, "widget-test-v1.json", `{
	"type": "object",
	"properties": {
		"spec": {"type": "object", "required": ["color"], "properties": {
			"size": {"type": "string"},
			"color": {"type": "string"}
		}}
	}
}`)

	var runErr error
	stdout := captureStdout(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:                  clusterDir,
			SchemaDir:             schemaDir,
			Output:                "json",
			disableDefaultSchemas: true,
		})
	})

	exitErr, ok := runErr.(*DiffExitError)
	if !ok {
		t.Fatalf("expected *DiffExitError, got %v", runErr)
	}
	if exitErr.ExitCode != ExitValidationFailed {
		t.Errorf("exit code = %d, want %d (ExitValidationFailed)", exitErr.ExitCode, ExitValidationFailed)
	}

	var report jsonValidationReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not a json report: %v\n%s", err, stdout)
	}
	if report.Summary.Invalid != 1 || report.Summary.Valid != 1 {
		t.Errorf("summary = %+v, want invalid=1 valid=1 (plus the schema-less Kustomization CR as skipped)\n%s", report.Summary, stdout)
	}

	// A valid run reports an empty report and no failure exit.
	writeHelper(t, appDir, "widgets.yaml", `apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: good-widget
spec:
  size: large
  color: red
`)
	runErr = nil
	stdout = captureStdout(func() {
		runErr = runValidate(context.Background(), &ValidateFlags{
			Path:                  clusterDir,
			SchemaDir:             schemaDir,
			Output:                "json",
			disableDefaultSchemas: true,
		})
	})
	if runErr != nil {
		t.Fatalf("expected success for a valid run, got: %v", runErr)
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not a json report: %v\n%s", err, stdout)
	}
	if report.Summary.Invalid != 0 || report.Summary.Valid != 1 {
		t.Errorf("summary = %+v, want invalid=0 valid=1", report.Summary)
	}
}

// TestRunValidate_OutputJSON_UncheckedOCISource: a Kustomization sourced
// from an OCIRepository whose path lives inside the artifact keeps the
// run green (exit 0) but must not read as fully checked — the unchecked
// Kustomization is carried in the report's "unchecked" array (#24) and
// named in the stderr warning.
func TestRunValidate_OutputJSON_UncheckedOCISource(t *testing.T) {
	repoRoot := t.TempDir()
	gitInit(t, repoRoot)

	clusterDir := filepath.Join(repoRoot, "cluster")
	writeHelper(t, clusterDir, "oci-ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: envoy-crds
  namespace: flux-system
spec:
  interval: 24h
  path: ./gateway-helm/charts/crds/crds
  prune: false
  sourceRef:
    kind: OCIRepository
    name: envoy-crds
`)
	writeHelper(t, clusterDir, "app.yaml", `apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: app-widget
spec:
  size: large
  color: red
`)

	// A local schema source: without one the validator reports no results
	// at all, and an empty report could not prove the local tree was
	// actually checked.
	schemaDir := filepath.Join(repoRoot, "schemas")
	if err := os.MkdirAll(schemaDir, 0755); err != nil {
		t.Fatalf("mkdir schema dir: %v", err)
	}
	writeHelper(t, schemaDir, "widget-test-v1.json", `{
	"type": "object",
	"properties": {
		"spec": {"type": "object", "required": ["color"], "properties": {
			"size": {"type": "string"},
			"color": {"type": "string"}
		}}
	}
}`)

	var runErr error
	var stderr string
	stdout := captureStdout(func() {
		stderr = captureStderr(func() {
			runErr = runValidate(context.Background(), &ValidateFlags{
				Path:                  clusterDir,
				SchemaDir:             schemaDir,
				Output:                "json",
				disableDefaultSchemas: true,
			})
		})
	})
	if runErr != nil {
		t.Fatalf("json output must stay green with an unchecked OCI source, got: %v\nstderr:\n%s", runErr, stderr)
	}
	var report jsonValidationReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not a json report: %v\n%s", err, stdout)
	}
	if report.Summary.Invalid != 0 {
		t.Errorf("summary = %+v, want invalid=0\n%s", report.Summary, stdout)
	}
	// The local tree must actually be in the report — a green summary on
	// an empty resource list would also "pass" the check above.
	var widgetSeen bool
	for _, r := range report.Resources {
		if r.Kind == "Widget" && r.Name == "app-widget" {
			widgetSeen = true
			if r.Status != string(validate.StatusValid) {
				t.Errorf("app-widget status = %s, want %s\n%s", r.Status, validate.StatusValid, stdout)
			}
		}
	}
	if !widgetSeen {
		t.Errorf("the local Widget app-widget must be in the report, got:\n%s", stdout)
	}
	if !strings.Contains(stderr, "left unchecked") {
		t.Errorf("the unchecked Kustomization must be named in stderr, got:\n%s", stderr)
	}

	// The machine-readable report carries the gap: one unchecked entry
	// naming the Kustomization and its source. The fixture holds no
	// OCIRepository manifest, so the cause is the missing object (#27).
	if len(report.Unchecked) != 1 {
		t.Fatalf("unchecked = %d entries, want 1:\n%s", len(report.Unchecked), stdout)
	}
	entry := report.Unchecked[0]
	if entry.Kustomization != "flux-system/envoy-crds" ||
		entry.Source != "OCIRepository flux-system/envoy-crds" ||
		!strings.Contains(entry.Cause, "not found in the repository") {
		t.Errorf("unchecked entry = %+v", entry)
	}
}

// TestRunValidate_OutputJUNIT_UncheckedOCISource: the junit timeline
// shows the gap as a skipped testcase instead of reading green (#24).
func TestRunValidate_OutputJUNIT_UncheckedOCISource(t *testing.T) {
	repoRoot := t.TempDir()
	gitInit(t, repoRoot)

	clusterDir := filepath.Join(repoRoot, "cluster")
	writeHelper(t, clusterDir, "oci-ks.yaml", `apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: envoy-crds
  namespace: flux-system
spec:
  interval: 24h
  path: ./gateway-helm/charts/crds/crds
  prune: false
  sourceRef:
    kind: OCIRepository
    name: envoy-crds
`)
	writeHelper(t, clusterDir, "app.yaml", `apiVersion: test.example.com/v1
kind: Widget
metadata:
  name: app-widget
spec:
  size: large
  color: red
`)

	schemaDir := filepath.Join(repoRoot, "schemas")
	if err := os.MkdirAll(schemaDir, 0755); err != nil {
		t.Fatalf("mkdir schema dir: %v", err)
	}
	writeHelper(t, schemaDir, "widget-test-v1.json", `{
	"type": "object",
	"properties": {
		"spec": {"type": "object", "required": ["color"], "properties": {
			"size": {"type": "string"},
			"color": {"type": "string"}
		}}
	}
}`)

	var runErr error
	var stderr string
	stdout := captureStdout(func() {
		stderr = captureStderr(func() {
			runErr = runValidate(context.Background(), &ValidateFlags{
				Path:                  clusterDir,
				SchemaDir:             schemaDir,
				Output:                "junit",
				disableDefaultSchemas: true,
			})
		})
	})
	if runErr != nil {
		t.Fatalf("junit output must stay green with an unchecked OCI source, got: %v\nstderr:\n%s", runErr, stderr)
	}

	var report parsedJUnit
	if err := xml.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not junit xml: %v\n%s", err, stdout)
	}
	if report.Failures != 0 || report.Errors != 0 {
		t.Errorf("failures = %d, errors = %d, want 0/0\n%s", report.Failures, report.Errors, stdout)
	}
	var uncheckedSeen bool
	for _, suite := range report.Suites {
		for _, tc := range suite.Cases {
			if tc.Name == "Kustomization flux-system/envoy-crds" {
				uncheckedSeen = true
				if tc.Skipped == nil {
					t.Errorf("the unchecked Kustomization must be a skipped testcase:\n%s", stdout)
				}
			}
		}
	}
	if !uncheckedSeen {
		t.Errorf("the unchecked Kustomization must appear in the junit report:\n%s", stdout)
	}
}

func TestRunValidate_UnknownOutputFormat(t *testing.T) {
	err := runValidate(context.Background(), &ValidateFlags{Output: "yaml"})
	exitErr, ok := err.(*DiffExitError)
	if !ok {
		t.Fatalf("expected *DiffExitError, got %v", err)
	}
	if exitErr.ExitCode != ExitCodeError {
		t.Errorf("exit code = %d, want %d (ExitCodeError)", exitErr.ExitCode, ExitCodeError)
	}
}

func TestWriteValidationJUnit_Unchecked(t *testing.T) {
	unchecked := []uncheckedGroup{
		{kustomizations: []string{"flux-system/envoy-crds"}, source: "OCIRepository flux-system/envoy", cause: "source kind OCIRepository is not fetched"},
	}
	var buf bytes.Buffer
	if err := writeValidationJUnit(&buf, rendererFixtures(), unchecked); err != nil {
		t.Fatal(err)
	}

	var report parsedJUnit
	if err := xml.Unmarshal(buf.Bytes(), &report); err != nil {
		t.Fatalf("output is not valid XML: %v\n%s", err, buf.String())
	}
	// 4 resources + 1 unchecked Kustomization.
	if report.Tests != 5 || report.Skipped != 2 {
		t.Errorf("counts = tests %d, skipped %d; want 5/2 (4 resources incl. 1 schema-less skip + 1 unchecked)", report.Tests, report.Skipped)
	}
	var found bool
	for _, suite := range report.Suites {
		for _, tc := range suite.Cases {
			if tc.Name == "Kustomization flux-system/envoy-crds" {
				found = true
				if tc.Skipped == nil {
					t.Errorf("the unchecked testcase must carry <skipped>:\n%s", buf.String())
				}
			}
		}
	}
	if !found {
		t.Errorf("the unchecked Kustomization must be a testcase:\n%s", buf.String())
	}
}
