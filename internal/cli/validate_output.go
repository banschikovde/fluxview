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

// jsonUncheckedKustomization is one Kustomization whose source content
// could not be obtained — its resources never reached the checked set.
type jsonUncheckedKustomization struct {
	Kustomization string `json:"kustomization"`
	Source        string `json:"source"`
	Cause         string `json:"cause"`
}

type jsonValidationReport struct {
	Resources []jsonValidationResource `json:"resources"`
	// Unchecked lists the Kustomizations left unchecked (unfetched
	// sources), in the stderr summary's group order; absent when the run
	// checked everything. The summary counts stay about resources.
	Unchecked []jsonUncheckedKustomization `json:"unchecked,omitempty"`
	Summary   jsonValidationSummary        `json:"summary"`
}

// writeValidationJSON writes the full validation report — every resource
// with its status, and the Kustomizations left unchecked — as indented
// JSON.
func writeValidationJSON(w io.Writer, results []validate.Result, unchecked []uncheckedGroup) error {
	report := jsonValidationReport{Resources: []jsonValidationResource{}}
	for _, g := range unchecked {
		for _, ks := range g.kustomizations {
			report.Unchecked = append(report.Unchecked, jsonUncheckedKustomization{
				Kustomization: ks,
				Source:        g.source,
				Cause:         g.cause,
			})
		}
	}
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
// errors as <error>, schema-less resources as <skipped>, and one skipped
// testcase per unchecked Kustomization — a partial check must show the
// gap on the CI timeline, not read as green.
func writeValidationJUnit(w io.Writer, results []validate.Result, unchecked []uncheckedGroup) error {
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
	uncheckedCases := 0
	for _, g := range unchecked {
		for _, ks := range g.kustomizations {
			suite.Cases = append(suite.Cases, junitCase{
				Classname: "Kustomization",
				Name:      "Kustomization " + ks,
				Skipped:   &junitSkipped{Message: "source not fetched: " + g.source + ": " + g.cause},
			})
			suite.Skipped++
			uncheckedCases++
		}
	}
	suite.Tests = len(results) + uncheckedCases

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
