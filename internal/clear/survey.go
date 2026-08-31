package clear

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mchmarny/aicrme/internal/aicrclient"
)

const (
	// surveyTimeout bounds the whole survey. It is a helm list plus one helm
	// history per matched release plus one kubectl get, all sequential, on a
	// screen an operator is waiting in front of. Without a ceiling a single
	// wedged apiserver call parks the panel indefinitely.
	surveyTimeout = 90 * time.Second
	// commandTimeout bounds one command, so one slow release cannot consume
	// the whole budget and starve the rest.
	commandTimeout = 30 * time.Second
)

// Survey is what is on the cluster, and how sure this console is about it.
type Survey struct {
	// ClusterUID is the cluster this describes, echoed by a later removal
	// request so that surveying one cluster, switching kubecontext and
	// confirming cannot act on a different one.
	ClusterUID string `json:"clusterUid"`
	// DriverMode warns whether removing gpu-operator here can require a node
	// reboot before the next install succeeds.
	DriverMode DriverMode `json:"driverMode"`
	// Complete is false when any evidence is missing. Nothing is recommended
	// on an incomplete survey, and the UI says so rather than rendering a
	// confident-looking list of unticked rows.
	Complete bool `json:"complete"`
	// Incomplete explains Complete=false, in the operator's terms.
	Incomplete string `json:"incomplete,omitempty"`
	// Releases is every matched release. One outside the universe is absent
	// entirely rather than listed and unticked: offering somebody else's
	// postgres invites removing a chart this console knows nothing about.
	Releases []Release `json:"releases"`
}

// Surveyor answers what AICR components are on a cluster. It never writes;
// wrap Exec in ReadOnly to make that structural.
type Surveyor struct {
	Exec   Exec
	Client aicrclient.API
}

// Survey reads the cluster and returns what it found.
//
// Ordered by the recipe's own component order, last-installed first, which is
// also the order a removal would run in.
func (s *Surveyor) Survey(ctx context.Context, clusterUID string) (*Survey, error) {
	ctx, cancel := context.WithTimeout(ctx, surveyTimeout)
	defer cancel()

	universe, err := aicrclient.Universe(ctx, s.Client)
	if err != nil {
		return nil, err
	}
	raw, err := listReleases(ctx, s.Exec)
	if err != nil {
		return nil, err
	}

	var reasons []string
	if !universe.Complete {
		reasons = append(reasons, "this console could not read "+strconv.Itoa(universe.Skipped)+
			" of AICR's recipe overlays, so a component installed here might not be recognised")
	}

	// Keyed by namespace/name, never name alone: two releases can share a name
	// in different namespaces, and keying on name silently drops one.
	order := map[string]int{}
	matched := make([]Release, 0, len(raw))
	for _, hr := range raw {
		comp, version, ok := matchChart(hr.Chart, universe.Charts)
		if !ok {
			continue
		}
		r := Release{
			Name:         hr.Name,
			Namespace:    hr.Namespace,
			Chart:        comp.Chart,
			ChartVersion: version,
			Component:    comp.Name,
			Revision:     atoiOrZero(hr.Revision),
			NodeLevel:    isNodeLevel(comp.Name, hr.Namespace),
		}
		if t, err := parseHelmTime(hr.Updated); err == nil {
			r.LastUpdated = t
		}
		hctx, hcancel := context.WithTimeout(ctx, commandTimeout)
		t, err := firstDeployed(hctx, s.Exec, hr.Name, hr.Namespace)
		hcancel()
		// t.IsZero() is checked alongside err: a zero time.Time is the
		// unreadable-history return value, not a legitimate answer, and
		// treating it as one would leave Complete true while recommend
		// still stamps every row with the incomplete-evidence reason --
		// a panel that contradicts its own banner.
		if err != nil || t.IsZero() {
			reasons = append(reasons, "this console could not read when "+hr.Namespace+"/"+hr.Name+" was first deployed")
		} else {
			r.FirstDeployed = t
		}
		order[hr.Namespace+"/"+hr.Name] = comp.Order
		matched = append(matched, r)
	}

	dctx, dcancel := context.WithTimeout(ctx, commandTimeout)
	mode := driverMode(dctx, s.Exec)
	dcancel()
	if mode == DriverUnknown {
		reasons = append(reasons, "this console could not tell whether the GPU Operator manages this cluster's NVIDIA driver, which decides whether the GPU nodes need rebooting after a removal")
	}

	complete := len(reasons) == 0
	out := recommend(matched, complete)
	sort.SliceStable(out, func(i, j int) bool {
		return order[out[i].Namespace+"/"+out[i].Name] > order[out[j].Namespace+"/"+out[j].Name]
	})

	return &Survey{
		ClusterUID: clusterUID,
		DriverMode: mode,
		Complete:   complete,
		Incomplete: strings.Join(reasons, "; "),
		Releases:   out,
	}, nil
}

func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
