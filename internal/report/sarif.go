package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/russellw/veritix/internal/audit"
)

// SARIF is the format code-scanning tools speak, and it is what lets a data
// audit appear in the same place as a security scan: annotated on a pull
// request, in GitHub's Security tab, or in any CI dashboard that already
// consumes it. Data quality is treated as a build concern by almost nobody,
// largely because it has never shown up where developers already look.
//
// https://docs.oasis-open.org/sarif/sarif/v2.1.0/sarif-v2.1.0.html

const (
	sarifVersion = "2.1.0"
	sarifSchema  = "https://json.schemastore.org/sarif-2.1.0.json"
)

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool    sarifTool     `json:"tool"`
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string      `json:"name"`
	Version        string      `json:"version,omitempty"`
	InformationURI string      `json:"informationUri,omitempty"`
	Rules          []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID               string            `json:"id"`
	Name             string            `json:"name,omitempty"`
	ShortDescription sarifText         `json:"shortDescription"`
	FullDescription  *sarifText        `json:"fullDescription,omitempty"`
	Help             *sarifText        `json:"help,omitempty"`
	Properties       map[string]string `json:"properties,omitempty"`
}

type sarifText struct {
	Text string `json:"text"`
}

type sarifResult struct {
	RuleID    string          `json:"ruleId"`
	Level     string          `json:"level"`
	Message   sarifText       `json:"message"`
	Locations []sarifLocation `json:"locations,omitempty"`
}

type sarifLocation struct {
	PhysicalLocation sarifPhysical `json:"physicalLocation"`
}

type sarifPhysical struct {
	ArtifactLocation sarifArtifact `json:"artifactLocation"`
	Region           *sarifRegion  `json:"region,omitempty"`
}

type sarifArtifact struct {
	URI string `json:"uri"`
}

type sarifRegion struct {
	StartLine int64 `json:"startLine"`
}

// WriteSARIF renders a run in SARIF 2.1.0.
func WriteSARIF(w io.Writer, res *audit.Result, version string, opts Options) error {
	doc := Build(res, version, opts)

	// SARIF separates the rule catalog from the results, so collect the
	// distinct rules first.
	seen := make(map[string]sarifRule)
	for _, f := range doc.Findings {
		if _, ok := seen[f.Rule]; ok {
			continue
		}
		r := sarifRule{
			ID:               f.Rule,
			Name:             f.Rule,
			ShortDescription: sarifText{Text: f.Title},
			Properties:       map[string]string{"origin": f.Origin},
		}
		if f.Detail != "" {
			r.FullDescription = &sarifText{Text: f.Detail}
		}
		if f.Remedy != "" {
			r.Help = &sarifText{Text: f.Remedy}
		}
		seen[f.Rule] = r
	}

	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	rules := make([]sarifRule, 0, len(ids))
	for _, id := range ids {
		rules = append(rules, seen[id])
	}

	results := make([]sarifResult, 0, len(doc.Findings))
	for _, f := range doc.Findings {
		msg := f.Title
		if f.Remedy != "" {
			msg += " " + f.Remedy
		}

		result := sarifResult{
			RuleID:  f.Rule,
			Level:   sarifLevel(f.Severity),
			Message: sarifText{Text: msg},
		}
		// A finding needs a file to be annotated against. Where it belongs to
		// a specific line, say so: that is what puts the comment on the right
		// row of a pull request.
		if f.File != "" {
			phys := sarifPhysical{ArtifactLocation: sarifArtifact{URI: f.File}}
			if f.Line > 0 {
				phys.Region = &sarifRegion{StartLine: f.Line}
			}
			result.Locations = []sarifLocation{{PhysicalLocation: phys}}
		}
		results = append(results, result)
	}

	log := sarifLog{
		Schema:  sarifSchema,
		Version: sarifVersion,
		Runs: []sarifRun{{
			Tool: sarifTool{Driver: sarifDriver{
				Name:           "Veritix",
				Version:        version,
				InformationURI: "https://github.com/russellw/veritix",
				Rules:          rules,
			}},
			Results: results,
		}},
	}

	enc := json.NewEncoder(w)
	if opts.Indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(log); err != nil {
		return fmt.Errorf("report: writing SARIF: %w", err)
	}
	return nil
}

// sarifLevel maps a Veritix severity onto SARIF's vocabulary.
func sarifLevel(severity string) string {
	switch severity {
	case "error":
		return "error"
	case "warning":
		return "warning"
	default:
		return "note"
	}
}
