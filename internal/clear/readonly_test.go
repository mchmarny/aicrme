package clear

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

type recorder struct{ argv [][]string }

func (r *recorder) Run(_ context.Context, argv []string, _ io.Writer) error {
	r.argv = append(r.argv, argv)
	return nil
}

// THE GUARD THIS FILE EXISTS FOR. Revision 1 asserted read-only with a
// blacklist in a test, which missed `helm install` and `helm upgrade` -- the
// two most likely things a future edit would reach for. A whitelist in
// PRODUCTION code cannot be defeated by forgetting to update a test.
func TestReadOnlyRefusesEveryMutatingCommand(t *testing.T) {
	for _, argv := range [][]string{
		{"helm", "uninstall", "gpu-operator"},
		{"helm", "install", "gpu-operator", "./chart"},
		{"helm", "upgrade", "--install", "gpu-operator", "./chart"},
		{"helm", "rollback", "gpu-operator"},
		{"kubectl", "delete", "ns", "gpu-operator"},
		{"kubectl", "apply", "-f", "x.yaml"},
		{"kubectl", "patch", "ns", "gpu-operator", "-p", "{}"},
		{"kubectl", "create", "ns", "x"},
		{"rm", "-rf", "/"},
	} {
		r := &recorder{}
		err := ReadOnly(r).Run(context.Background(), argv, io.Discard)
		if !errors.Is(err, ErrNotReadOnly) {
			t.Errorf("ReadOnly allowed %v (err = %v)", argv, err)
		}
		if len(r.argv) != 0 {
			t.Errorf("ReadOnly forwarded %v to the underlying exec", argv)
		}
	}
}

func TestReadOnlyAllowsTheSurveysOwnCommands(t *testing.T) {
	for _, argv := range [][]string{
		{"helm", "list", "-A", "-o", "json"},
		{"helm", "history", "gpu-operator", "-n", "gpu-operator", "-o", "json"},
		{"kubectl", "get", "daemonset", "-A"},
	} {
		r := &recorder{}
		if err := ReadOnly(r).Run(context.Background(), argv, io.Discard); err != nil {
			t.Errorf("ReadOnly refused %v: %v", argv, err)
		}
		if len(r.argv) != 1 {
			t.Errorf("ReadOnly did not forward %v", argv)
		}
	}
}

// A one-word or empty argv must be refused rather than indexed into.
func TestReadOnlyRefusesAnArgvItCannotClassify(t *testing.T) {
	for _, argv := range [][]string{{}, {"helm"}} {
		if err := ReadOnly(&recorder{}).Run(context.Background(), argv, io.Discard); !errors.Is(err, ErrNotReadOnly) {
			t.Errorf("ReadOnly allowed unclassifiable argv %v", argv)
		}
	}
}

func TestReadOnlyPassesOutputThrough(t *testing.T) {
	e := ReadOnly(execReturning("hello"))
	var buf bytes.Buffer
	if err := e.Run(context.Background(), []string{"helm", "list"}, &buf); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if buf.String() != "hello" {
		t.Errorf("output = %q, want it passed through", buf.String())
	}
}

type execReturning string

func (s execReturning) Run(_ context.Context, _ []string, out io.Writer) error {
	_, err := io.WriteString(out, string(s))
	return err
}
