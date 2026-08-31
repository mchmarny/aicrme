package clear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// installWindow is how far apart two releases can be first deployed and still
// be judged part of the same install. One hour, against an install that takes
// roughly ten minutes on real hardware.
const installWindow = time.Hour

// Release is one matched helm release and everything known about it.
//
// The evidence fields are not decoration. Chart matching cannot distinguish
// this project's gpu-operator from another team's -- they are the same chart --
// so the operator's judgement is the safety mechanism, and these fields are
// what it is exercised on.
type Release struct {
	Name         string `json:"name"`
	Namespace    string `json:"namespace"`
	Chart        string `json:"chart"`
	ChartVersion string `json:"chartVersion"`
	// Component is AICR's identifier for the chart, shown because an operator
	// reading a recipe sees that name and not the chart's.
	Component string `json:"component"`
	Revision  int    `json:"revision"`
	// FirstDeployed is revision 1's timestamp. Zero when helm's history could
	// not be read, which makes the whole survey's evidence incomplete.
	FirstDeployed time.Time `json:"firstDeployed"`
	LastUpdated   time.Time `json:"lastUpdated"`
	// NodeLevel marks a component whose effects outlive its release: Skyhook
	// packages write GRUB parameters and sysctls that deleting a CR does not
	// revert. Never recommended, and increment 2 gives it no checkbox.
	NodeLevel bool `json:"nodeLevel"`
	// Recommended is this console's suggestion, not its decision.
	Recommended bool `json:"recommended"`
	// Reason is why Recommended is false. Stated rather than implied: an
	// operator cannot act on a silence.
	Reason string `json:"reason,omitempty"`
}

// helmHistoryEntry is `helm history -o json`, where revision is a NUMBER --
// unlike `helm list -o json`, where the same field is a string.
type helmHistoryEntry struct {
	Revision int    `json:"revision"`
	Updated  string `json:"updated"`
	Status   string `json:"status"`
}

// firstDeployed returns revision 1's timestamp for one release.
//
// One extra command per matched release, and worth it: this is the only signal
// separating a component installed with the rest of a bundle from one that has
// been on the cluster for months and was merely upgraded recently.
func firstDeployed(ctx context.Context, e Exec, name, namespace string) (time.Time, error) {
	var buf bytes.Buffer
	if err := e.Run(ctx, []string{"helm", "history", name, "-n", namespace, "-o", "json"}, &buf); err != nil {
		return time.Time{}, fmt.Errorf("reading history for %s/%s: %w", namespace, name, err)
	}
	var entries []helmHistoryEntry
	if err := json.Unmarshal(buf.Bytes(), &entries); err != nil {
		return time.Time{}, fmt.Errorf("parsing history for %s/%s: %w", namespace, name, err)
	}
	for _, h := range entries {
		if h.Revision == 1 {
			return parseHelmTime(h.Updated)
		}
	}
	return time.Time{}, fmt.Errorf("no revision 1 in history for %s/%s", namespace, name)
}

// nodeLevelComponents are the charts whose install modifies the node itself.
// The skyhook namespace counts as its own signal because AICR's own tooling
// identifies the install that way (`tools/cleanup --exclude-ns skyhook`), and
// a release renamed by whoever installed it would slip a name-only check.
var nodeLevelComponents = []string{"nodewright"}

const nodeLevelNamespace = "skyhook"

func isNodeLevel(component, namespace string) bool {
	if namespace == nodeLevelNamespace {
		return true
	}
	for _, p := range nodeLevelComponents {
		if strings.HasPrefix(component, p) {
			return true
		}
	}
	return false
}

const incompleteEvidenceReason = "this console could not establish when every component here was first deployed, so it cannot tell which of them belong to one install"

// recommend fills in Recommended and Reason for every release.
//
// PARTIAL EVIDENCE RECOMMENDS NOTHING. The rule is proximity to the newest
// matched release, and that anchor is only meaningful if every release's
// first-deployed date is known. An earlier version of this function dropped
// unreadable releases from the comparison set, which computed the anchor from
// a subset -- so an older release could be recommended against an anchor that
// was simply wrong. Any gap now disables the whole rule and says so.
//
// The rule is not `revision == 1`: a retry re-runs `helm upgrade --install`
// and produces revision 2, so that would drop exactly the component whose
// install needed retrying.
func recommend(rs []Release, evidenceComplete bool) []Release {
	if len(rs) == 0 {
		return rs
	}
	var newest time.Time
	for _, r := range rs {
		if r.FirstDeployed.IsZero() {
			evidenceComplete = false
		}
		if r.FirstDeployed.After(newest) {
			newest = r.FirstDeployed
		}
	}
	out := make([]Release, 0, len(rs))
	for _, r := range rs {
		switch {
		case r.NodeLevel:
			r.Recommended = false
			r.Reason = "node-level: removing this leaves the node's kernel parameters and sysctls exactly as they are, with nothing left that knows how to revert them"
		case !evidenceComplete:
			r.Recommended = false
			r.Reason = incompleteEvidenceReason
		case newest.Sub(r.FirstDeployed) > installWindow:
			r.Recommended = false
			r.Reason = fmt.Sprintf("first deployed %s, %s before the rest of this install",
				r.FirstDeployed.Format("2006-01-02"), humanGap(newest.Sub(r.FirstDeployed)))
		default:
			r.Recommended = true
		}
		out = append(out, r)
	}
	return out
}

// humanGap renders a duration the way the sentence around it reads, because
// "5064h0m0s before the rest of this install" is not a sentence.
func humanGap(d time.Duration) string {
	days := int(d.Hours() / 24)
	switch {
	case days >= 60:
		return fmt.Sprintf("%d months", days/30)
	case days >= 1:
		return fmt.Sprintf("%d days", days)
	default:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
}
