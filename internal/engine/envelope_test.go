package engine

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/base64"
	"sort"
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

// incompressibleBytes returns n bytes no DEFLATE window can crush. A periodic
// fill (e.g. byte(i*7)) is exactly what LZ77 eats -- a 4MiB buffer like that
// gzips to ~16KiB and never trips maxPayload -- so every oversize fixture
// below needs a genuinely non-repeating source.
func incompressibleBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand.Read() error = %v", err)
	}
	return b
}

// TestEncodeShedsOversizedArtifactsRatherThanFailing is I5's subject: an
// oversized record must still be persistable. Failing the encode instead made
// Decide -- the one mandatory checkpoint -- fail deterministically, roll the
// run back to awaiting_decision, and leave Discard refusing because that
// state is live, with no operator action anywhere that reached a working
// console. Shedding the artifact that will not fit degrades durability
// instead of the product.
func TestEncodeShedsOversizedArtifactsRatherThanFailing(t *testing.T) {
	in := testRun()
	in.Artifacts["snapshot.yaml"] = incompressibleBytes(t, 1<<20)
	in.Artifacts["recipe.json"] = []byte(`{"components":[{"name":"gpu-operator"}]}`)

	blob, err := encodeRun(in)
	if err != nil {
		t.Fatalf("encodeRun() error = %v, want the oversized artifact shed rather than the write failed", err)
	}
	if len(blob) > maxPayload {
		t.Errorf("encoded size %d exceeds maxPayload %d -- shedding must actually bring the record under the cap", len(blob), maxPayload)
	}

	out, err := decodeRun(blob)
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}
	// The state machine is what recovery actually needs, and all of it must
	// survive: a truncated record that lost StepIndex or Decisions would be
	// worse than no record at all.
	if out.ID != in.ID || out.State != in.State || out.Phase != in.Phase || out.StepIndex != in.StepIndex {
		t.Errorf("scalar fields drifted on a truncated record: %+v", out)
	}
	if out.Decisions["intent"] != "inference" {
		t.Errorf("Decisions = %v, want them preserved on a truncated record", out.Decisions)
	}
	if len(out.Components) != 1 || out.Components[0].Name != "nfd" {
		t.Errorf("Components = %v, want the projection preserved on a truncated record", out.Components)
	}
	// Largest first: the 1MiB snapshot goes, the 40-byte recipe stays.
	if _, ok := out.Artifacts["snapshot.yaml"]; ok {
		t.Error("snapshot.yaml survived, want the largest artifact shed first")
	}
	if _, ok := out.Artifacts["recipe.json"]; !ok {
		t.Error("recipe.json was shed, want shedding to stop as soon as the record fits -- recovery rewinds to Bundle, which reads exactly this artifact")
	}
	if in.Artifacts["snapshot.yaml"] == nil {
		t.Error("encodeRun mutated the caller's run")
	}
}

// The record must say what is missing rather than looking complete: a
// truncated envelope that decoded as an ordinary one would present a run as
// fully recoverable when a retry is going to fail on the first artifact read.
func TestEncodeNamesTheArtifactsItShed(t *testing.T) {
	in := testRun()
	in.Artifacts["snapshot.yaml"] = incompressibleBytes(t, 1<<20)

	blob, err := encodeRun(in)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	var env envelope
	if err := gunzipJSON(blob, &env); err != nil {
		t.Fatalf("gunzipJSON() error = %v", err)
	}
	if len(env.Truncated) != 1 || env.Truncated[0] != "snapshot.yaml" {
		t.Errorf("Truncated = %v, want exactly [snapshot.yaml]", env.Truncated)
	}
}

// A record that fits keeps Truncated empty: the marker must mean something,
// not be set on every write.
func TestEncodeLeavesTruncatedEmptyWhenTheRecordFits(t *testing.T) {
	blob, err := encodeRun(testRun())
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	var env envelope
	if err := gunzipJSON(blob, &env); err != nil {
		t.Fatalf("gunzipJSON() error = %v", err)
	}
	if len(env.Truncated) != 0 {
		t.Errorf("Truncated = %v, want empty for a record that fits", env.Truncated)
	}
}

// Shedding must be deterministic, not map-iteration-order dependent: two
// saves of the same run that dropped different artifacts would make a
// recovered record's contents unpredictable. Two equally oversized artifacts
// leave exactly one survivor, and it must be the same one every time.
func TestEncodeShedsDeterministically(t *testing.T) {
	blobs := make([]string, 0, 5)
	for range 5 {
		in := testRun()
		big := incompressibleBytes(t, 600<<10)
		// Same length, so only the tie-break on key name can decide.
		in.Artifacts["aaa.bin"] = big
		in.Artifacts["zzz.bin"] = append([]byte(nil), big...)

		blob, err := encodeRun(in)
		if err != nil {
			t.Fatalf("encodeRun() error = %v", err)
		}
		out, err := decodeRun(blob)
		if err != nil {
			t.Fatalf("decodeRun() error = %v", err)
		}
		keys := make([]string, 0, len(out.Artifacts))
		for k := range out.Artifacts {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		blobs = append(blobs, strings.Join(keys, ","))
	}
	for i, got := range blobs {
		if got != blobs[0] {
			t.Fatalf("run %d shed a different set (%q) than run 0 (%q) -- shedding depends on map iteration order", i, got, blobs[0])
		}
	}
}

// The guard still fails closed where shedding cannot help: nothing here is an
// artifact, so there is nothing to drop, and silently persisting a record
// missing pieces of the state machine itself would be worse than refusing.
func TestEncodeRejectsOversizedPayload(t *testing.T) {
	in := testRun()
	in.Artifacts = map[string][]byte{}
	// Incompressible decision values: base64 of random bytes uses 64 symbols,
	// so DEFLATE recovers at most ~25% and 2MiB of it stays well past the cap.
	big := incompressibleBytes(t, 2<<20)
	in.Decisions = map[string]string{"intent": base64.RawStdEncoding.EncodeToString(big)}

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
