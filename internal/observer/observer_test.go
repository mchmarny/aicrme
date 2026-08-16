package observer_test

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/mchmarny/aicrme/internal/bus"
	"github.com/mchmarny/aicrme/internal/observer"
)

// collect drains the bus into a slice until deadline, so a test can assert
// both what was published and that nothing was. A single-goroutine select
// loop is the only writer to out, so no mutex is needed to guard it.
func collect(t *testing.T, b *bus.Bus, within time.Duration) []bus.Event {
	t.Helper()
	sub, unsub := b.Subscribe(0)
	t.Cleanup(unsub)
	var (
		out  []bus.Event
		done = time.After(within)
	)
	for {
		select {
		case e := <-sub:
			out = append(out, e)
		case <-done:
			return out
		}
	}
}

func scopeFor(ns ...string) func() observer.RunScope {
	set := make(map[string]struct{}, len(ns))
	for _, n := range ns {
		set[n] = struct{}{}
	}
	return func() observer.RunScope { return observer.RunScope{RunID: "run-1", Namespaces: set} }
}

func daemonSet(ns, name string, ready, desired int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: "ds-uid"},
		Status: appsv1.DaemonSetStatus{
			NumberReady:            ready,
			DesiredNumberScheduled: desired,
		},
	}
}

func TestDaemonSetRolloutProgressIsNarrated(t *testing.T) {
	client := fake.NewSimpleClientset(daemonSet("gpu-operator", "nvidia-driver-daemonset", 0, 8))
	b := bus.New(256)
	o := observer.New(client, b, scopeFor("gpu-operator"))

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	ds := daemonSet("gpu-operator", "nvidia-driver-daemonset", 2, 8)
	if _, err := client.AppsV1().DaemonSets("gpu-operator").Update(
		context.Background(), ds, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	events := collect(t, b, 2*time.Second)
	var found bool
	for _, e := range events {
		if strings.Contains(e.Message, "nvidia-driver-daemonset 2/8 nodes ready") {
			found = true
			if e.Kind != bus.KindCluster {
				t.Errorf("Kind = %q, want %q", e.Kind, bus.KindCluster)
			}
			if e.RunID != "run-1" {
				t.Errorf("RunID = %q, want run-1", e.RunID)
			}
			if !strings.Contains(e.Message, "gpu-operator/") {
				t.Errorf("Message = %q, want it namespace-qualified", e.Message)
			}
		}
	}
	if !found {
		t.Fatalf("no rollout event published; got %d events", len(events))
	}
}

// The property the whole design rests on. An update that changes nothing the
// observer reports must publish nothing -- informer UpdateFunc fires on
// managedFields and annotation churn, and the bus drops subscribers 256
// events behind.
func TestUnchangedStateEmitsNothing(t *testing.T) {
	initial := daemonSet("gpu-operator", "nvidia-driver-daemonset", 2, 8)
	client := fake.NewSimpleClientset(initial)
	b := bus.New(256)
	o := observer.New(client, b, scopeFor("gpu-operator"))

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	noisy := daemonSet("gpu-operator", "nvidia-driver-daemonset", 2, 8)
	noisy.Annotations = map[string]string{"kubectl.kubernetes.io/restartedAt": "now"}
	if _, err := client.AppsV1().DaemonSets("gpu-operator").Update(
		context.Background(), noisy, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	if events := collect(t, b, time.Second); len(events) != 0 {
		t.Fatalf("published %d events for an unchanged rollout, want 0: %+v", len(events), events)
	}
}

// The informer's initial list delivers every existing object as an Add.
// Emitting there would narrate the cluster's entire pre-existing state at
// pod start.
func TestInitialListEmitsNothing(t *testing.T) {
	client := fake.NewSimpleClientset(daemonSet("gpu-operator", "nvidia-driver-daemonset", 8, 8))
	b := bus.New(256)
	o := observer.New(client, b, scopeFor("gpu-operator"))

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if events := collect(t, b, time.Second); len(events) != 0 {
		t.Fatalf("published %d events for the initial list, want 0: %+v", len(events), events)
	}
}

func TestWorkloadsOutsideTheRunScopeAreIgnored(t *testing.T) {
	client := fake.NewSimpleClientset(daemonSet("kube-system", "some-other-ds", 0, 3))
	b := bus.New(256)
	o := observer.New(client, b, scopeFor("gpu-operator"))

	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	ds := daemonSet("kube-system", "some-other-ds", 1, 3)
	if _, err := client.AppsV1().DaemonSets("kube-system").Update(
		context.Background(), ds, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	if events := collect(t, b, time.Second); len(events) != 0 {
		t.Fatalf("published %d events for an out-of-scope namespace, want 0", len(events))
	}
}

func TestNilClientYieldsANoOpObserver(t *testing.T) {
	o := observer.New(nil, bus.New(8), scopeFor())
	stop := make(chan struct{})
	defer close(stop)
	if err := o.Start(stop); err != nil {
		t.Fatalf("Start() with nil client error = %v, want nil (degrade, not fail)", err)
	}
}
