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
	// InferredGPUs marks counts obtained by multiplying one node's probe by
	// the number of GPU nodes, rather than counted from what the nodes
	// advertise. It is an assumption that every GPU node is identical, which
	// AICR itself warns against: its GPU collector samples a single node and
	// logs when topology reports non-uniform GPU labels across the cluster.
	// The UI says so rather than presenting a guess as a measurement.
	InferredGPUs bool `json:"inferredGpus,omitempty"`
	// Analyzed is false when Analyze had no usable snapshot. Gaps is empty
	// in two very different situations -- every capability already present,
	// and nothing measured at all -- and the console must not show the
	// green "already capable" copy for the second. The headline strings
	// differ too, but keying UI on prose is how prose changes become bugs.
	Analyzed bool `json:"analyzed"`
	// Simulated is true when kwok-controller is running in this cluster, which
	// means some or all of its nodes are fakes. It is deliberately NOT derived
	// from the GPU count: KWOK's fake nodes advertise eight GPUs each, so a
	// simulated cluster and a real one are indistinguishable by that measure.
	// Anything that would act on the cluster rather than describe it has to
	// know the difference -- see internal/steps.skipReason.
	Simulated bool `json:"simulated"`
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

// ClusterGPUs is what the caller learned by walking the node list, and is the
// only cluster-wide GPU evidence that exists.
//
// The snapshot cannot supply this. Its GPU measurement comes from an in-pod
// PCI probe of the single node the agent landed on, and AICR's topology
// summary carries node-count, taint-count and label-count but no GPU count.
// So a caller with a node list knows something Analyze cannot derive.
//
// The zero value means "no node list", and Analyze then behaves exactly as it
// did before this type existed. It is an explicit parameter rather than an
// option so that every call site has to say what it knows -- the undercount
// this fixes survived precisely because nothing forced that question.
type ClusterGPUs struct {
	// Nodes is how many nodes advertise GPUs at all.
	Nodes int
	// Total and Usable are capacity and allocatable, summed cluster-wide.
	// Both are zero before a device plugin publishes nvidia.com/gpu, which is
	// the ordinary state of a cluster this console is about to configure.
	Total  int64
	Usable int64
}

// Analyze produces the capability statement and gap list. A nil or empty
// snapshot yields a renderable report rather than a panic — the UI must always
// have something to show.
func Analyze(s *aicr.Snapshot, cluster ClusterGPUs) Report {
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
	total, usable, inferred := resolveGPUs(p, cluster)
	report := Report{
		Headline:     headline(p, total),
		Detail:       detail(p),
		TotalGPUs:    total,
		UsableGPUs:   usable,
		InferredGPUs: inferred,
		Analyzed:     true,
		Simulated:    kwokControllerPresent(p),
	}
	for _, rule := range rules {
		if rule.Applies(p) {
			report.Gaps = append(report.Gaps, Gap{
				ID: rule.ID, Title: rule.Title, Detail: rule.Detail, Component: rule.Component,
			})
		}
	}
	report.Punchline = punchline(report)
	return report
}

// resolveGPUs picks the best evidence available for the cluster's GPU counts.
//
// Three tiers, best first:
//
//  1. What the nodes advertise — counted, cluster-wide, exact.
//  2. One node's probe multiplied by the number of GPU nodes — a guess, marked
//     as one, for the pre-device-plugin cluster where tier 1 has nothing to
//     say. This is exactly the cluster this console exists to configure.
//  3. The probe alone: one node's truth, which is all that existed before this
//     function and remains right for a caller with no node list.
//
// Both numbers move together in every tier, and that is the whole point.
// Taking the denominator from the node list while leaving the numerator on the
// probe turns "8 of 8" into "8 of 16" on a perfectly healthy sixteen-GPU
// cluster — inventing a fault, which is worse than the undercount it replaces.
func resolveGPUs(p probe, c ClusterGPUs) (total, usable int, inferred bool) {
	if c.Total > 0 {
		return int(c.Total), int(c.Usable), false
	}
	probeTotal, probeUsable := totalGPUs(p), usableGPUs(p)
	if c.Nodes > 1 && probeTotal > 0 {
		return probeTotal * c.Nodes, probeUsable * c.Nodes, true
	}
	return probeTotal, probeUsable, false
}

func punchline(r Report) string {
	if r.TotalGPUs == 0 {
		return "No GPU hardware detected — this is a simulated cluster."
	}
	if r.InferredGPUs {
		return fmt.Sprintf("%d of %d GPUs are usable by a workload today, inferred from one node.",
			r.UsableGPUs, r.TotalGPUs)
	}
	return fmt.Sprintf("%d of %d GPUs are usable by a workload today.", r.UsableGPUs, r.TotalGPUs)
}
