package engine

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

func testRun() *Run {
	return &Run{
		ID:         "abc123",
		State:      StateRunning,
		Phase:      PhaseApply,
		Decisions:  map[string]string{"intent": "inference"},
		Pending:    []string{"apply"},
		StepIndex:  3,
		StartedAt:  time.Now().UTC().Truncate(time.Second),
		UpdatedAt:  time.Now().UTC().Truncate(time.Second),
		Components: []ComponentState{{Name: "nfd", Index: 2, Total: 14, Status: "installed"}},
		Artifacts:  map[string][]byte{"snapshot.yaml": bytes.Repeat([]byte("a: b\n"), 100)},
	}
}

func TestEncodeDecodeRoundTripsArtifacts(t *testing.T) {
	in := testRun()
	blob, err := encodeRun(in)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	out, err := decodeRun(blob)
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}
	// Artifacts are the point: Run.Artifacts is json:"-", so a naive
	// json.Marshal(run) would round-trip everything here EXCEPT the one field
	// recovery cannot work without.
	if !bytes.Equal(out.Artifacts["snapshot.yaml"], in.Artifacts["snapshot.yaml"]) {
		t.Errorf("snapshot.yaml did not survive: got %d bytes, want %d",
			len(out.Artifacts["snapshot.yaml"]), len(in.Artifacts["snapshot.yaml"]))
	}
	if out.ID != in.ID || out.State != in.State || out.Phase != in.Phase || out.StepIndex != in.StepIndex {
		t.Errorf("scalar fields drifted: %+v", out)
	}
	if len(out.Components) != 1 || out.Components[0].Name != "nfd" {
		t.Errorf("Components = %v, want one nfd row", out.Components)
	}
	if out.Decisions["intent"] != "inference" {
		t.Errorf("Decisions = %v", out.Decisions)
	}
}

func TestEncodeDropsBundlePath(t *testing.T) {
	in := testRun()
	in.Artifacts["bundle.path"] = []byte("/var/lib/aicrme/runs/abc123/bundle")
	blob, err := encodeRun(in)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	out, err := decodeRun(blob)
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}
	if _, ok := out.Artifacts["bundle.path"]; ok {
		t.Error("bundle.path survived encoding -- it points into an emptyDir that " +
			"does not survive a restart, so restoring it aims Apply at a vanished directory")
	}
	if in.Artifacts["bundle.path"] == nil {
		t.Error("encodeRun mutated the caller's run")
	}
}

func TestEncodeCompresses(t *testing.T) {
	in := testRun()
	// Highly compressible, like a real snapshot.yaml.
	in.Artifacts["snapshot.yaml"] = bytes.Repeat([]byte("nodes:\n  - name: gpu-0\n"), 4000)
	raw := len(in.Artifacts["snapshot.yaml"])
	blob, err := encodeRun(in)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	if len(blob) >= raw/4 {
		t.Errorf("encoded size %d is not meaningfully smaller than raw %d -- "+
			"compression is the headroom against the 1MiB ConfigMap cap", len(blob), raw)
	}
}

func TestEncodeRejectsOversizedPayload(t *testing.T) {
	in := testRun()
	// Incompressible, so it cannot be squeezed under the cap. A periodic
	// fill (e.g. byte(i*7)) is exactly what DEFLATE's LZ77 window crushes --
	// a 4MiB buffer like that gzips to ~16KiB and never trips maxPayload, so
	// this needs a genuinely non-repeating source.
	big := make([]byte, 4<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatalf("rand.Read() error = %v", err)
	}
	in.Artifacts["snapshot.yaml"] = big
	if _, err := encodeRun(in); err == nil {
		t.Fatal("encodeRun() error = nil, want ErrTooLarge")
	} else if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error = %v, want a too-large error", err)
	}
}

func TestDecodeRejectsUnknownVersion(t *testing.T) {
	blob, err := encodeRun(testRun())
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	bumped := bumpVersionForTest(t, blob)
	if _, err := decodeRun(bumped); err == nil {
		t.Fatal("decodeRun() error = nil, want an unsupported-version error")
	}
}

func TestDecodeBoundsDecompression(t *testing.T) {
	// A gzip bomb: small stored, enormous expanded. The pod is capped at
	// 512Mi, so an unbounded reader here is an OOM kill rather than an error.
	bomb := gzipBombForTest(t, 64<<20)
	if _, err := decodeRun(bomb); err == nil {
		t.Fatal("decodeRun() error = nil, want a decode error from the size bound")
	}
}

// bumpVersionForTest rewrites the envelope's version to one the decoder does
// not know, without hand-authoring a fixture that would drift from the type.
func bumpVersionForTest(t *testing.T, blob []byte) []byte {
	t.Helper()
	var env envelope
	if err := gunzipJSON(blob, &env); err != nil {
		t.Fatalf("gunzipJSON() error = %v", err)
	}
	env.Version = envelopeVersion + 99
	out, err := gzipJSON(env)
	if err != nil {
		t.Fatalf("gzipJSON() error = %v", err)
	}
	return out
}

func gzipBombForTest(t *testing.T, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(make([]byte, size)); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return buf.Bytes()
}
