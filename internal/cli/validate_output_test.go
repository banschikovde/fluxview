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
	if err := writeValidationJSON(&buf, rendererFixtures()); err != nil {
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
}

func TestWriteValidationJSON_EmptyRun(t *testing.T) {
	var buf bytes.Buffer
	if err := writeValidationJSON(&buf, nil); err != nil {
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
	if err := writeValidationJUnit(&buf, rendererFixtures()); err != nil {
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
