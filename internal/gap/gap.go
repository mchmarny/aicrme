// Package gap turns an AICR cluster snapshot into the capability statement and
// gap list that open the console. Each gap names the component that closes it,
// so the Discover screen pre-explains the Apply screen.
package gap

import (
	"fmt"

	aicr "github.com/NVIDIA/aicr/pkg/client/v1"
	"github.com/NVIDIA/aicr/pkg/measurement"
)

// Gap is one missing capability.
type Gap struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Detail    string `json:"detail,omitempty"`
	Component string `json:"component"`
}

// Report is the full Discover payload.
type Report struct {
	Headline   string `json:"headline"`
	Detail     string `json:"detail,omitempty"`
	Punchline  string `json:"punchline"`
	Gaps       []Gap  `json:"gaps"`
	UsableGPUs int    `json:"usableGpus"`
	TotalGPUs  int    `json:"totalGpus"`
}

// probe is the read-only view the rules evaluate against.
type probe struct {
	measurements []*measurement.Measurement
}

func (p probe) measurement(t measurement.Type) *measurement.Measurement {
	for _, m := range p.measurements {
		if m.Type == t {
			return m
		}
	}
	return nil
}

// Analyze produces the capability statement and gap list. A nil or empty
// snapshot yields a renderable report rather than a panic — the UI must always
// have something to show.
func Analyze(s *aicr.Snapshot) Report {
	if s == nil {
		return Report{
			Headline:  "No cluster snapshot available.",
			Punchline: "Run Discover to capture the cluster's current state.",
		}
	}
	inner := s.Unwrap()
	if inner == nil {
		return Report{
			Headline:  "No cluster snapshot available.",
			Punchline: "Run Discover to capture the cluster's current state.",
		}
	}

	p := probe{measurements: inner.Measurements}
	report := Report{
		Headline:  headline(p),
		Detail:    detail(p),
		TotalGPUs: totalGPUs(p),
	}
	for _, rule := range rules {
		if rule.Applies(p) {
			report.Gaps = append(report.Gaps, Gap{
				ID: rule.ID, Title: rule.Title, Detail: rule.Detail, Component: rule.Component,
			})
		}
	}
	report.UsableGPUs = usableGPUs(p)
	report.Punchline = punchline(report)
	return report
}

func punchline(r Report) string {
	if r.TotalGPUs == 0 {
		return "No GPU hardware detected — this is a simulated cluster."
	}
	return fmt.Sprintf("%d of %d GPUs are usable by a workload today.", r.UsableGPUs, r.TotalGPUs)
}
