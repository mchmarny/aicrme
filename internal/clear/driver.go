package clear

import (
	"bytes"
	"context"
	"strings"
)

// DriverMode is who installs the NVIDIA kernel driver on this cluster.
type DriverMode string

const (
	// DriverManaged means gpu-operator installs the driver (driver.enabled=true):
	// EKS, and every platform whose bring-up does not ship one.
	DriverManaged DriverMode = "managed"
	// DriverHost means the driver arrives with the infrastructure, as it does
	// on GKE's COS images.
	DriverHost DriverMode = "host"
	// DriverUnknown means the probe could not answer. A THIRD ANSWER, NOT A
	// DEFAULT: collapsing a failed probe into "host" would hide the most
	// consequential warning on the screen at exactly the moment this console
	// has least information.
	DriverUnknown DriverMode = "unknown"
)

// driverMode reports whether gpu-operator manages the NVIDIA kernel driver.
//
// WHY THIS MATTERS MORE THAN ANYTHING ELSE THE SURVEY REPORTS. On a
// driver-managed cluster, uninstalling gpu-operator can leave the nvidia_uvm
// kernel module wedged mid-unload on GPU nodes, and the NEXT install then
// fails driver validation (Init:CrashLoopBackOff, "failed to create device
// node nvidia-uvm") until those nodes are rebooted. AICR documents this in
// tools/cleanup and states that automating cordon/drain/reboot is
// cloud-specific and out of scope; it is out of scope here too, so the console
// warns instead -- BEFORE the operator decides, not after.
//
// The probe is the nvidia-driver-daemonset, AICR's own primary signal: the
// concrete artifact of driver-managed mode, needing no extra tooling and
// robust to release-name drift. Searched cluster-wide by name because the
// operator's namespace is not fixed -- Talos recipes install gpu-operator into
// privileged-gpu-operator.
//
// AICR also has a `helm get values gpu-operator -a` fallback for the window
// where the DaemonSet is not yet listable. Not implemented: this survey runs
// against an already-installed cluster, where the DaemonSet exists if the mode
// does, and a second probe would buy only the mid-install case never seen here.
func driverMode(ctx context.Context, e Exec) DriverMode {
	var buf bytes.Buffer
	err := e.Run(ctx, []string{
		"kubectl", "get", "daemonset", "-A",
		"-o", `jsonpath={.items[?(@.metadata.name=="nvidia-driver-daemonset")].metadata.namespace}`,
		"--request-timeout=15s",
	}, &buf)
	if err != nil {
		return DriverUnknown
	}
	if strings.TrimSpace(buf.String()) == "" {
		return DriverHost
	}
	return DriverManaged
}
