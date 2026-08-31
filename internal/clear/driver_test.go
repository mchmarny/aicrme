package clear

import (
	"context"
	"errors"
	"testing"
)

// The warning this enables is the most consequential thing the survey says:
// removing gpu-operator where it manages the driver can leave nvidia_uvm
// wedged mid-unload, and the NEXT install then fails driver validation until
// the GPU nodes are rebooted (NVIDIA/aicr#1553).
func TestDriverModeIsManagedWhenTheDriverDaemonSetExists(t *testing.T) {
	e := &fakeExec{out: map[string]string{"kubectl get daemonset": "gpu-operator\n"}}
	if got := driverMode(context.Background(), e); got != DriverManaged {
		t.Errorf("driverMode = %q, want %q", got, DriverManaged)
	}
}

func TestDriverModeIsHostWhenNoDriverDaemonSetExists(t *testing.T) {
	e := &fakeExec{out: map[string]string{"kubectl get daemonset": "\n"}}
	if got := driverMode(context.Background(), e); got != DriverHost {
		t.Errorf("driverMode = %q, want %q; GKE ships a host driver", got, DriverHost)
	}
}

// UNKNOWN IS A THIRD ANSWER, NOT A DEFAULT. Collapsing a failed probe into
// "host" hides the most consequential warning on the screen at exactly the
// moment this console has least information -- which is the wrong direction
// for a warning to fail.
func TestDriverModeIsUnknownWhenTheProbeFails(t *testing.T) {
	e := &fakeExec{err: map[string]error{"kubectl get daemonset": errors.New("timeout")}}
	if got := driverMode(context.Background(), e); got != DriverUnknown {
		t.Errorf("driverMode = %q, want %q -- a failed probe is not evidence of a host driver", got, DriverUnknown)
	}
}

func TestDriverModeRunsOnlyAGet(t *testing.T) {
	e := &fakeExec{out: map[string]string{"kubectl get daemonset": "gpu-operator\n"}}
	driverMode(context.Background(), e)
	if len(e.argv) != 1 || e.argv[0][1] != "get" {
		t.Errorf("argv = %v, want a single get", e.argv)
	}
}
