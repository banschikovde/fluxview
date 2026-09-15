package cli

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"strings"

	"github.com/banschikovde/fluxview/internal/validate"
)

// Rendering of validation results: human-readable text (stderr, the
// default) and machine-readable json/junit (stdout) for CI.

// writeValidationText writes the human-readable failure listing.
func writeValidationText(w io.Writer, failures []validate.Result) {
	for _, r := range failures {
		fmt.Fprintf(w, "✗ %s\n", r.Label())
		for _, e := range r.Errors {
			fmt.Fprintf(w, "  %s\n", e)
		}
	}
}

// --- JSON ---

type jsonValidationResource struct {
	Kind      string   `json:"kind"`
	Name      string   `json:"name"`
	Namespace string   `json:"namespace,omitempty"`
	Status    string   `json:"status"`
	Errors    []string `json:"errors,omitempty"`
}

type jsonValidationSummary struct {
	Valid   int `json:"valid"`
	Skipped int `json:"skipped"`
	Invalid int `json:"invalid"`
	Errors  int `json:"errors"`
}

type jsonValidationReport struct {
	Resources []jsonValidationResource `json:"resources"`
	Summary   jsonValidationSummary    `json:"summary"`
}

// writeValidationJSON writes the full validation report — every resource
// with its status — as indented JSON.
func writeValidationJSON(w io.Writer, results []validate.Result) error {
	report := jsonValidationReport{Resources: []jsonValidationResource{}}
	for _, r := range results {
		res := jsonValidationResource{
			Kind:      r.Kind,
			Name:      r.Name,
			Namespace: r.Namespace,
			Status:    string(r.Status),
			Errors:    r.Errors,
		}
		if res.Kind == "" {
			res.Kind = "malformed"
		}
		report.Resources = append(report.Resources, res)

		switch r.Status {
		case validate.StatusValid:
			report.Summary.Valid++
		case validate.StatusSkipped:
			report.Summary.Skipped++
		case validate.StatusInvalid:
			report.Summary.Invalid++
		case validate.StatusError:
			report.Summary.Errors++
		}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// --- JUnit ---

type junitFailure struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

type junitError struct {
	Message string `xml:"message,attr"`
	Text    string `xml:",chardata"`
}

type junitSkipped struct {
	Message string `xml:"message,attr"`
}

type junitCase struct {
	Classname string        `xml:"classname,attr,omitempty"`
	Name      string        `xml:"name,attr"`
	Failure   *junitFailure `xml:"failure,omitempty"`
	Err       *junitError   `xml:"error,omitempty"`
	Skipped   *junitSkipped `xml:"skipped,omitempty"`
}

type junitSuite struct {
	Name     string      `xml:"name,attr"`
	Tests    int         `xml:"tests,attr"`
	Failures int         `xml:"failures,attr"`
	Errors   int         `xml:"errors,attr"`
	Skipped  int         `xml:"skipped,attr"`
	Cases    []junitCase `xml:"testcase"`
}

type junitReport struct {
	XMLName  xml.Name     `xml:"testsuites"`
	Name     string       `xml:"name,attr"`
	Tests    int          `xml:"tests,attr"`
	Failures int          `xml:"failures,attr"`
	Errors   int          `xml:"errors,attr"`
	Skipped  int          `xml:"skipped,attr"`
	Suites   []junitSuite `xml:"testsuite"`
}

// writeValidationJUnit writes the validation report in JUnit XML: one
// testcase per resource, failed validation as <failure>, validation
// errors as <error>, schema-less resources as <skipped>.
func writeValidationJUnit(w io.Writer, results []validate.Result) error {
	suite := junitSuite{Name: "fluxview validate"}
	for _, r := range results {
		tc := junitCase{Classname: r.Kind, Name: r.Label()}
		switch r.Status {
		case validate.StatusInvalid:
			tc.Failure = &junitFailure{
				Message: fmt.Sprintf("%d validation error(s)", len(r.Errors)),
				Text:    strings.Join(r.Errors, "\n"),
			}
			suite.Failures++
		case validate.StatusError:
			tc.Err = &junitError{
				Message: "validation error",
				Text:    strings.Join(r.Errors, "\n"),
			}
			suite.Errors++
		case validate.StatusSkipped:
			tc.Skipped = &junitSkipped{Message: "no schema or skipped kind"}
			suite.Skipped++
		}
		suite.Cases = append(suite.Cases, tc)
	}
	suite.Tests = len(results)

	report := junitReport{
		Name:     "fluxview validate",
		Tests:    suite.Tests,
		Failures: suite.Failures,
		Errors:   suite.Errors,
		Skipped:  suite.Skipped,
		Suites:   []junitSuite{suite},
	}

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(report); err != nil {
		return err
	}
	return enc.Flush()
}
