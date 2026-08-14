package gap

import (
	"fmt"
	"strings"

	"github.com/NVIDIA/aicr/pkg/measurement"
)

// Subtype names read from the fixture. These are producer-defined strings —
// see github.com/NVIDIA/aicr/pkg/measurement's catalog — not exported
// constants, so they are pinned here where they are used.
const (
	subtypeGPUHardware        = "hardware" // TypeGPU
	subtypeK8sNode            = "node"     // TypeK8s
	subtypeK8sServer          = "server"   // TypeK8s
	subtypeOSRelease          = "release"  // TypeOS
	subtypeNodeTopologySumary = "summary"  // TypeNodeTopology
)

// readingTrue is the text form Scalar[bool].String() produces for a true
// Reading — Get(key).String() is how a bool-typed Data value (e.g.
// driver-loaded, gpu-present) is compared, since GetString rejects non-string
// Readings.
const readingTrue = "true"

// rule is one gap detector. Applies returns true when the capability is absent.
type rule struct {
	ID        string
	Title     string
	Detail    string
	Component string
	Applies   func(probe) bool
}

// rules is deliberately short. Task 9's brief names five candidate gaps — GPU
// driver, device plugin, GPU-aware scheduler, EFA plugin, GPU metrics — but a
// KWOK snapshot (internal/gap/testdata/snapshot-kwok.yaml, captured live from
// the aicr-kwok-test cluster with `aicr snapshot`) only proves one of them:
//
//   - gpu-driver: TypeGPU's "hardware" subtype carries driver-loaded, and the
//     fixture has it false. Provable.
//   - device plugin, GPU-aware scheduler, GPU metrics: no producer in AICR's
//     measurement catalog (pkg/measurement/catalog.go) emits a subtype for
//     any of these — there is no addressable path a snapshot could ever carry
//     that would prove or disprove them. Not just absent from KWOK; absent
//     from the schema.
//   - EFA plugin: would need a TypeNetworkTopology measurement (subtypes
//     identity/capabilities/pfs carry rdma/ib/rail data), but this snapshot —
//     control-plane-only KWOK, no `--discover-network` — has no
//     TypeNetworkTopology measurement at all. The Type exists in the schema
//     but this fixture never populates it.
//
// Shipping a rule for any of the last four would mean guessing at an "absent"
// verdict no evidence backs — exactly what the brief says not to do. They
// wait on a real EKS fixture (Phase 4), where the device plugin, scheduler,
// EFA plugin and DCGM exporter either are or are not actually installed.
var rules = []rule{
	{
		ID:        "gpu-driver",
		Title:     "No GPU driver installed, the kernel does not see the devices",
		Detail:    "GPU.hardware reports driver-loaded=false: the NVIDIA driver is not loaded, so nvidia-smi and the device nodes are unavailable.",
		Component: "gpu-operator",
		Applies: func(p probe) bool {
			m := p.measurement(measurement.TypeGPU)
			if m == nil {
				return false
			}
			st := m.GetSubtype(subtypeGPUHardware)
			if st == nil || !st.Has(measurement.KeyGPUDriverLoaded) {
				return false
			}
			// driver-loaded is a bool reading, not a string one — GetString
			// would fail with ErrCodeInvalidRequest against it. Get().String()
			// round-trips any scalar Reading to its text form regardless of
			// underlying type.
			return st.Get(measurement.KeyGPUDriverLoaded).String() != readingTrue
		},
	},
}

// providerName reads K8s.node's provider field, e.g. "kind" or "eks".
func providerName(p probe) string {
	m := p.measurement(measurement.TypeK8s)
	if m == nil {
		return ""
	}
	st := m.GetSubtype(subtypeK8sNode)
	if st == nil {
		return ""
	}
	v, err := st.GetString("provider")
	if err != nil {
		return ""
	}
	return v
}

// k8sServerVersion reads K8s.server's version field.
func k8sServerVersion(p probe) string {
	m := p.measurement(measurement.TypeK8s)
	if m == nil {
		return ""
	}
	st := m.GetSubtype(subtypeK8sServer)
	if st == nil {
		return ""
	}
	v, err := st.GetString(measurement.KeyVersion)
	if err != nil {
		return ""
	}
	return v
}

// osPrettyName reads OS.release's PRETTY_NAME field, e.g. "Ubuntu 22.04.4 LTS".
func osPrettyName(p probe) string {
	m := p.measurement(measurement.TypeOS)
	if m == nil {
		return ""
	}
	st := m.GetSubtype(subtypeOSRelease)
	if st == nil {
		return ""
	}
	v, err := st.GetString("PRETTY_NAME")
	if err != nil {
		return ""
	}
	return v
}

// nodeCount reads NodeTopology.summary's node-count field.
func nodeCount(p probe) int {
	m := p.measurement(measurement.TypeNodeTopology)
	if m == nil {
		return 0
	}
	st := m.GetSubtype(subtypeNodeTopologySumary)
	if st == nil {
		return 0
	}
	v, err := st.GetInt64("node-count")
	if err != nil {
		return 0
	}
	return int(v)
}

// totalGPUs reads GPU.hardware's gpu-count field. Zero on a snapshot with no
// GPU measurement at all, or with a measurement that never detected hardware —
// both render the same way, since neither result is a usable GPU.
func totalGPUs(p probe) int {
	m := p.measurement(measurement.TypeGPU)
	if m == nil {
		return 0
	}
	st := m.GetSubtype(subtypeGPUHardware)
	if st == nil {
		return 0
	}
	v, err := st.GetInt64(measurement.KeyGPUCount)
	if err != nil {
		return 0
	}
	return int(v)
}

// usableGPUs is the subset of totalGPUs a workload could actually schedule
// onto today: hardware present and the driver loaded. This is a lower bound
// derived from the same "hardware" subtype the gpu-driver rule reads — the
// snapshot carries no device-plugin or scheduler signal to refine it further.
func usableGPUs(p probe) int {
	total := totalGPUs(p)
	if total == 0 {
		return 0
	}
	m := p.measurement(measurement.TypeGPU)
	if m == nil {
		return 0
	}
	st := m.GetSubtype(subtypeGPUHardware)
	if st == nil {
		return 0
	}
	if !st.Has(measurement.KeyGPUPresent) || st.Get(measurement.KeyGPUPresent).String() != readingTrue {
		return 0
	}
	if !st.Has(measurement.KeyGPUDriverLoaded) || st.Get(measurement.KeyGPUDriverLoaded).String() != readingTrue {
		return 0
	}
	return total
}

func headline(p probe) string {
	provider := providerName(p)
	if provider == "" {
		provider = "the"
	}
	if gpus := totalGPUs(p); gpus > 0 {
		return fmt.Sprintf("This is a %s cluster with %d GPUs.", provider, gpus)
	}
	return fmt.Sprintf("This is a %s cluster with %d node(s) and no GPU hardware detected.", provider, nodeCount(p))
}

func detail(p probe) string {
	var parts []string
	if v := k8sServerVersion(p); v != "" {
		parts = append(parts, "Kubernetes "+v)
	}
	if v := osPrettyName(p); v != "" {
		parts = append(parts, v)
	}
	return strings.Join(parts, ", ")
}
