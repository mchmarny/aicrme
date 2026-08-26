package engine

import (
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/base64"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// testPayloadCeiling is the ConfigMap store's old 800 KiB ceiling, kept here
// after that store was deleted because the envelope's shedding behavior needs
// a ceiling a test-sized record can actually reach. filePayloadCeiling is 64
// MiB precisely so shedding is unreachable in normal use, which makes it
// useless for testing the thing shedding does.
const testPayloadCeiling = 800 << 10

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
	blob, err := encodeRun(in, testPayloadCeiling)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	out, err := decodeRun(blob, testPayloadCeiling)
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
	blob, err := encodeRun(in, testPayloadCeiling)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	out, err := decodeRun(blob, testPayloadCeiling)
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
	blob, err := encodeRun(in, testPayloadCeiling)
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
// gzips to ~16KiB and never trips the payload ceiling -- so every oversize fixture
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

	blob, err := encodeRun(in, testPayloadCeiling)
	if err != nil {
		t.Fatalf("encodeRun() error = %v, want the oversized artifact shed rather than the write failed", err)
	}
	if len(blob) > testPayloadCeiling {
		t.Errorf("encoded size %d exceeds testPayloadCeiling %d -- shedding must actually bring the record under the cap", len(blob), testPayloadCeiling)
	}

	out, err := decodeRun(blob, testPayloadCeiling)
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
		t.Error("recipe.json was shed, want shedding to stop as soon as the record fits -- largest-first exists to minimize what is lost, not to keep the run retryable (Bundle reads snapshot.yaml too, so a truncated run cannot be retried at all)")
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

	blob, err := encodeRun(in, testPayloadCeiling)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	var env envelope
	if err := gunzipJSON(blob, &env, testPayloadCeiling*10); err != nil {
		t.Fatalf("gunzipJSON() error = %v", err)
	}
	if len(env.Truncated) != 1 || env.Truncated[0] != "snapshot.yaml" {
		t.Errorf("Truncated = %v, want exactly [snapshot.yaml]", env.Truncated)
	}
}

// A record that fits keeps Truncated empty: the marker must mean something,
// not be set on every write.
func TestEncodeLeavesTruncatedEmptyWhenTheRecordFits(t *testing.T) {
	blob, err := encodeRun(testRun(), testPayloadCeiling)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	var env envelope
	if err := gunzipJSON(blob, &env, testPayloadCeiling*10); err != nil {
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

		blob, err := encodeRun(in, testPayloadCeiling)
		if err != nil {
			t.Fatalf("encodeRun() error = %v", err)
		}
		out, err := decodeRun(blob, testPayloadCeiling)
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

	if _, err := encodeRun(in, testPayloadCeiling); err == nil {
		t.Fatal("encodeRun() error = nil, want ErrTooLarge")
	} else if !strings.Contains(err.Error(), "too large") {
		t.Errorf("error = %v, want a too-large error", err)
	}
}

func TestDecodeRejectsUnknownVersion(t *testing.T) {
	blob, err := encodeRun(testRun(), testPayloadCeiling)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	bumped := bumpVersionForTest(t, blob)
	if _, err := decodeRun(bumped, testPayloadCeiling); err == nil {
		t.Fatal("decodeRun() error = nil, want an unsupported-version error")
	}
}

func TestDecodeBoundsDecompression(t *testing.T) {
	// A gzip bomb: small stored, enormous expanded. The pod is capped at
	// 512Mi, so an unbounded reader here is an OOM kill rather than an error.
	bomb := gzipBombForTest(t, 64<<20)
	if _, err := decodeRun(bomb, testPayloadCeiling); err == nil {
		t.Fatal("decodeRun() error = nil, want a decode error from the size bound")
	}
}

// TestDerivedDecompressionBoundNeverNarrowsTheConfigMapStoresOriginal pins
// the invariant fix round 1 found broken: the ×10 this file originally used
// made testPayloadCeiling*10 (8,192,000) narrower than the unparameterized
// code's original 8 << 20 (8,388,608), so decodeRun would have newly
// rejected a decompressed record between roughly 7.8 and 8 MiB that the old
// bound accepted. TestDecodeBoundsDecompression's 64 MiB bomb is nowhere
// near either threshold, so a several-percent narrowing like that is
// invisible to it -- this asserts the derivation arithmetic directly instead
// of constructing a payload near the boundary, which would be far more
// brittle (a single byte of JSON/gzip framing overhead either way changes
// whether a near-boundary fixture actually exercises the bound).
func TestDerivedDecompressionBoundNeverNarrowsTheConfigMapStoresOriginal(t *testing.T) {
	got := testPayloadCeiling * decompressMultiplier
	const oldBound = 8 << 20
	if got < oldBound {
		t.Errorf("derived decompression bound at testPayloadCeiling = %d, want >= %d (the ConfigMap store's original 8 MiB) -- "+
			"parameterizing the ceiling must never cause decodeRun to reject a record the old, unparameterized bound accepted", got, oldBound)
	}
}

// bumpVersionForTest rewrites the envelope's version to one the decoder does
// not know, without hand-authoring a fixture that would drift from the type.
func bumpVersionForTest(t *testing.T, blob []byte) []byte {
	t.Helper()
	var env envelope
	if err := gunzipJSON(blob, &env, testPayloadCeiling*10); err != nil {
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

// TestTruncatedSurvivesTheRoundTrip is what makes the console able to say a
// retry cannot work. The record was already honest about the loss --
// envelope.Truncated named it -- but decodeRun dropped the field on the way
// out, so nothing in the process knew, and the recovery panel offered "Retry
// this run" for a record whose retry fails at the first step reading a
// dropped key.
func TestTruncatedSurvivesTheRoundTrip(t *testing.T) {
	in := testRun()
	in.Artifacts["snapshot.yaml"] = incompressibleBytes(t, 1<<20)

	blob, err := encodeRun(in, testPayloadCeiling)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	out, err := decodeRun(blob, testPayloadCeiling)
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}
	if len(out.Truncated) != 1 || out.Truncated[0] != "snapshot.yaml" {
		t.Errorf("Run.Truncated = %v, want [snapshot.yaml] -- a decoded record must carry its own incompleteness", out.Truncated)
	}
	if len(in.Truncated) != 0 {
		t.Error("encodeRun mutated the caller's run")
	}
}

// A record that fits leaves Run.Truncated empty, so the flag means something
// rather than being set on every load.
func TestTruncatedEmptyOnAnIntactRecord(t *testing.T) {
	blob, err := encodeRun(testRun(), testPayloadCeiling)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	out, err := decodeRun(blob, testPayloadCeiling)
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}
	if len(out.Truncated) != 0 {
		t.Errorf("Run.Truncated = %v, want empty for an intact record", out.Truncated)
	}
}

// The loss must be sticky across saves. A run recovered from a truncated
// record no longer holds the shed artifact, so re-encoding it fits on the
// first try -- and if Truncated were recomputed rather than carried forward,
// the very next checkpoint would produce a record claiming completeness while
// still missing everything the first truncation dropped.
func TestTruncatedIsCarriedForwardOnRewrite(t *testing.T) {
	in := testRun()
	in.Artifacts["snapshot.yaml"] = incompressibleBytes(t, 1<<20)

	blob, err := encodeRun(in, testPayloadCeiling)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	recovered, err := decodeRun(blob, testPayloadCeiling)
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}

	// The record this process would write next, from the run it recovered.
	rewritten, err := encodeRun(recovered, testPayloadCeiling)
	if err != nil {
		t.Fatalf("encodeRun(recovered) error = %v", err)
	}
	out, err := decodeRun(rewritten, testPayloadCeiling)
	if err != nil {
		t.Fatalf("decodeRun(rewritten) error = %v", err)
	}
	if len(out.Truncated) != 1 || out.Truncated[0] != "snapshot.yaml" {
		t.Errorf("Run.Truncated after a rewrite = %v, want [snapshot.yaml] still named", out.Truncated)
	}
}

// TestCleanupUnconfirmedSurvivesTheRoundTrip is fix round 2's N1 unit-level
// pin, alongside the fuller Recover-based
// TestUnconfirmedCleanupSurvivesRestart in recover_test.go: envelope.go is a
// hand-maintained projection, and this field went a full fix round without a
// producer here, silently dropping Ruling 12's guard across a restart.
func TestCleanupUnconfirmedSurvivesTheRoundTrip(t *testing.T) {
	in := testRun()
	in.State = StateFailed
	in.CleanupUnconfirmed = true

	blob, err := encodeRun(in, testPayloadCeiling)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	out, err := decodeRun(blob, testPayloadCeiling)
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}
	if !out.CleanupUnconfirmed {
		t.Error("CleanupUnconfirmed = false after the round trip, want true")
	}
}

// A run that never had an unconfirmed cleanup must decode as false, not pick
// up junk from an unset field.
func TestCleanupUnconfirmedFalseOnAnOrdinaryRecord(t *testing.T) {
	blob, err := encodeRun(testRun(), testPayloadCeiling)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	out, err := decodeRun(blob, testPayloadCeiling)
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}
	if out.CleanupUnconfirmed {
		t.Error("CleanupUnconfirmed = true for a record that never set it, want false")
	}
}

// TestDecodeRunAcceptsARecordWithoutCleanupUnconfirmed is the backward-
// compatibility check workload_test.go's TestDecodeRunAcceptsARecordWithoutWorkload
// already establishes the pattern for: a ConfigMap written by a build before
// this field existed has no "cleanupUnconfirmed" key at all, and a rollout
// that adds the field must not turn that record unreadable -- it must decode
// as false, the only value a pre-Ruling-12 record could ever have meant.
func TestDecodeRunAcceptsARecordWithoutCleanupUnconfirmed(t *testing.T) {
	old := envelopeBeforeWorkload{
		Version:   envelopeVersion,
		ID:        "run-old",
		State:     StateFailed,
		Phase:     PhaseApply,
		StartedAt: time.Now().UTC().Truncate(time.Second),
		UpdatedAt: time.Now().UTC().Truncate(time.Second),
	}
	blob, err := gzipJSON(old)
	if err != nil {
		t.Fatalf("gzipJSON() error = %v", err)
	}
	out, err := decodeRun(blob, testPayloadCeiling)
	if err != nil {
		t.Fatalf("decodeRun() error = %v, want a pre-CleanupUnconfirmed record to still decode", err)
	}
	if out.CleanupUnconfirmed {
		t.Errorf("CleanupUnconfirmed = true for a record with no cleanupUnconfirmed field, want false")
	}
}

// runFieldsExcludedFromEnvelope names every exported Run field that
// envelope deliberately does NOT carry through the round trip, with why.
// Empty today -- nothing on Run has ever needed the exclusion. Fix round
// 3's Ruling 20: this list existing (even empty) is the point. A field
// added here without a stated reason fails TestEnvelopeRoundTripsEveryRunField
// below just as loudly as a field never added at all -- deliberate
// exclusions stay legal, they just have to be a decision made in the open,
// not a gap nobody was looking at.
var runFieldsExcludedFromEnvelope = map[string]string{
	// "FieldName": "why envelope must not carry it",
}

// setDistinctFieldValue sets v (one field of a zero-value Run, addressed via
// reflection) to a value that is guaranteed non-zero and, for collection
// types, non-empty -- so a field encodeRun forgets to populate decodes back
// to its Go zero value and a straight equality check below catches it. Kept
// as an explicit, small type switch rather than a fully generic reflection
// walker: Run has a fixed, short list of field types today, and a future
// field of a type this function does not handle fails loudly (t.Fatalf)
// rather than silently comparing two zero values as "equal" and reporting a
// false pass.
func setDistinctFieldValue(t *testing.T, v reflect.Value, name string) {
	t.Helper()
	// if/else on v.Kind(), not a switch: golangci-lint's exhaustive linter
	// requires every reflect.Kind case even with a default arm (the same
	// reason internal/prove/client.go's PlacedNodes compares Pod phases
	// with if/else rather than switching on corev1.PodPhase), and this
	// function only ever needs to handle the handful of kinds Run's own
	// field types actually use.
	switch {
	case v.Kind() == reflect.String:
		v.SetString("parity-" + name)
	case v.Kind() == reflect.Int:
		v.SetInt(7)
	case v.Kind() == reflect.Bool:
		v.SetBool(true)
	case v.Kind() == reflect.Struct:
		switch v.Interface().(type) {
		case time.Time:
			v.Set(reflect.ValueOf(time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)))
		case Workload:
			v.Set(reflect.ValueOf(Workload{Namespace: "parity-ns", Kind: "Job", Name: "parity-workload"}))
		case Residue:
			// Incomplete true and a fully-populated item: the guard and the
			// inventory are separate producers in envelope.go's projection,
			// and losing either one has its own distinct consequence.
			v.Set(reflect.ValueOf(Residue{
				Incomplete: true,
				Items: []ResidueItem{{
					Kind: KindRelease, Name: "parity-release", Namespace: "parity-ns",
					Removed: true, Skip: "parity-skip", Err: "parity-err",
				}},
			}))
		case AgentNamespace:
			// Every field distinct from its zero value: an envelope that
			// carried the name but dropped Created would otherwise round-trip
			// a DeepEqual-identical value on a run that never created one.
			v.Set(reflect.ValueOf(AgentNamespace{
				Name: "parity-agent-ns", UID: "parity-uid", Created: true,
			}))
		case Ownership:
			// Both slices non-empty, and every NamespaceRef field distinct
			// from its zero value -- an envelope that carried Ownership but
			// dropped, say, SnapshotErr would otherwise round-trip a
			// DeepEqual-identical value and report a false pass.
			v.Set(reflect.ValueOf(Ownership{
				Releases: []ReleaseRef{{Name: "parity-release", Namespace: "parity-release-ns"}},
				Namespaces: []NamespaceRef{{
					Name: "parity-ns", Existed: true, SnapshotErr: "parity-err",
				}},
			}))
		default:
			t.Fatalf("setDistinctFieldValue: field %s has unhandled struct type %s -- extend this switch", name, v.Type())
		}
	case v.Kind() == reflect.Map:
		et := v.Type().Elem()
		switch {
		case et.Kind() == reflect.String:
			v.Set(reflect.ValueOf(map[string]string{"k": "v"}))
		case et.Kind() == reflect.Slice && et.Elem().Kind() == reflect.Uint8:
			v.Set(reflect.ValueOf(map[string][]byte{"k": []byte("v")}))
		default:
			t.Fatalf("setDistinctFieldValue: field %s has unhandled map type %s -- extend this switch", name, v.Type())
		}
	case v.Kind() == reflect.Slice:
		et := v.Type().Elem()
		switch et {
		case reflect.TypeOf(""):
			v.Set(reflect.ValueOf([]string{"a"}))
		case reflect.TypeOf(ComponentState{}):
			v.Set(reflect.ValueOf([]ComponentState{{Name: "nfd", Index: 1, Total: 2, Status: "installed"}}))
		default:
			t.Fatalf("setDistinctFieldValue: field %s has unhandled slice element type %s -- extend this switch", name, et)
		}
	default:
		t.Fatalf("setDistinctFieldValue: field %s has unhandled kind %s -- extend this switch", name, v.Kind())
	}
}

// TestEnvelopeRoundTripsEveryRunField is fix round 3's Ruling 20. The
// reviewer's own demonstration is why this has to be a ROUND TRIP, not a
// weaker check that only compares the two structs' field NAMES: dropping
// BOTH the `Pending: r.Pending` and `Err: r.Err` lines from encodeRun's
// populate-literal (a 2-line diff) leaves the entire module green, because
// envelope still DECLARES both fields -- encodeRun just stops assigning
// them. A name-only comparison sees two structs that both have a "Pending"
// and an "Err" field and reports no problem; only an actual round trip
// notices the value never survived. envelope.go is a hand-maintained
// projection, there are over a hundred internal/engine call sites that
// touch a Run without ever comparing it against envelope, and nothing
// before this checked Run<->envelope parity at all -- this is what would
// have caught fix round 2's N1 (Run.CleanupUnconfirmed shipping without an
// envelope producer) before it shipped, and catches the next one for free.
func TestEnvelopeRoundTripsEveryRunField(t *testing.T) {
	in := &Run{}
	rv := reflect.ValueOf(in).Elem()
	rt := rv.Type()
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		setDistinctFieldValue(t, rv.Field(i), f.Name)
	}

	blob, err := encodeRun(in, testPayloadCeiling)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	out, err := decodeRun(blob, testPayloadCeiling)
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}
	outV := reflect.ValueOf(out).Elem()

	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		if reason, excluded := runFieldsExcludedFromEnvelope[f.Name]; excluded {
			if reason == "" {
				t.Errorf("Run.%s is in runFieldsExcludedFromEnvelope with no stated reason", f.Name)
			}
			continue
		}

		inField := rv.Field(i).Interface()
		outField := outV.Field(i).Interface()
		if inTime, ok := inField.(time.Time); ok {
			// time.Time round-trips through JSON's RFC 3339 encoding, which
			// does not preserve the monotonic reading reflect.DeepEqual
			// would otherwise compare -- Equal is the correct comparison,
			// the same one time.Time's own package doc recommends over ==.
			outTime, _ := outField.(time.Time)
			if !inTime.Equal(outTime) {
				t.Errorf("Run.%s = %v after the round trip, want %v", f.Name, outTime, inTime)
			}
			continue
		}
		if !reflect.DeepEqual(inField, outField) {
			t.Errorf("Run.%s = %+v after the round trip, want %+v -- envelope.go must carry this field "+
				"(encodeRun/decodeRun), or add it to runFieldsExcludedFromEnvelope with a stated reason",
				f.Name, outField, inField)
		}
	}
}

// A larger ceiling must actually keep artifacts a smaller one would shed.
// This is the whole reason the ceiling became a parameter: the ConfigMap's
// 800 KiB is a Kubernetes object limit, not a property of a run.
func TestEncodeRunHonorsTheCeilingItIsGiven(t *testing.T) {
	run := &Run{ID: "abcdef0123456789", State: StateRunning, Artifacts: map[string][]byte{
		"big.json": incompressibleBytes(t, 900<<10),
	}}

	small, err := encodeRun(run, 800<<10)
	if err != nil {
		t.Fatalf("encodeRun(small) error = %v", err)
	}
	shed, err := decodeRun(small, 800<<10)
	if err != nil {
		t.Fatalf("decodeRun(small) error = %v", err)
	}
	if len(shed.Truncated) == 0 {
		t.Error("the 800 KiB ceiling shed nothing from a 900 KiB artifact")
	}

	large, err := encodeRun(run, 64<<20)
	if err != nil {
		t.Fatalf("encodeRun(large) error = %v", err)
	}
	kept, err := decodeRun(large, 64<<20)
	if err != nil {
		t.Fatalf("decodeRun(large) error = %v", err)
	}
	if len(kept.Truncated) != 0 {
		t.Errorf("the 64 MiB ceiling shed %v -- shedding must be unreachable at the file store's ceiling", kept.Truncated)
	}
}

func baseRunForEnvelope(t *testing.T) *Run {
	t.Helper()
	now := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	return &Run{ID: "0123456789abcdef", State: StateDone, Phase: PhaseApply, StartedAt: now, UpdatedAt: now}
}

// The ownership snapshot is the only evidence that separates a release this
// console created from one it adopted via `helm upgrade --install`, and it
// is worthless if it does not survive a restart -- Reset runs long after
// Apply, frequently in a different pod.
func TestEnvelopeRoundTripsOwnership(t *testing.T) {
	in := baseRunForEnvelope(t)
	in.Ownership = Ownership{
		Releases: []ReleaseRef{{Name: "gpu-operator", Namespace: "gpu-operator"}},
		Namespaces: []NamespaceRef{
			{Name: "gpu-operator", Existed: true},
			{Name: "kai-scheduler", Existed: false},
			{Name: "monitoring", Existed: false, SnapshotErr: "connection refused"},
		},
	}

	blob, err := encodeRun(in, testPayloadCeiling)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	out, err := decodeRun(blob, testPayloadCeiling)
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}
	if !reflect.DeepEqual(out.Ownership, in.Ownership) {
		t.Errorf("Ownership round-tripped as %#v, want %#v", out.Ownership, in.Ownership)
	}
}

// TestEnvelopeRoundTripsEveryComponentStateField is the nested half of
// Ruling 20's parity guard. The top-level test above walks Run's own
// fields, so a value dropped from ComponentState -- which Run carries as a
// slice -- is invisible to it: it would persist as its zero value, survive
// the whole suite, and surface as a Reset that uninstalls from the wrong
// namespace after a restart. That is the CleanupUnconfirmed defect exactly
// (fix round 2's N1), one level of nesting down.
//
// What actually holds today, stated because it is what decides whether this
// test can bite: envelope.Components is []ComponentState, the SAME type, so
// a field ADDED to ComponentState is carried for free and this test cannot
// fail for that reason -- verified by adding a field and watching it still
// pass. The hazard it does catch is the one envelope.go's own doc comment
// invites: the moment envelope forks a parallel component type, or encodeRun
// projects the slice through anything that drops a field, this fails and
// names the field. Verified by making encodeRun blank one nested field and
// confirming the failure message.
//
// The engine's other nested projection, bootstrapComponentData in
// recover.go, is guarded by the compiler instead: recover.go converts
// ComponentState to it directly, so divergent fields are a build error, not
// a silent drop. Whoever adds a field to ComponentState updates both.
func TestEnvelopeRoundTripsEveryComponentStateField(t *testing.T) {
	var cs ComponentState
	rv := reflect.ValueOf(&cs).Elem()
	rt := rv.Type()
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		setDistinctFieldValue(t, rv.Field(i), f.Name)
	}

	in := &Run{
		ID:         "0123456789abcdef",
		State:      StateDone,
		Phase:      PhaseApply,
		StartedAt:  time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC),
		UpdatedAt:  time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC),
		Components: []ComponentState{cs},
	}

	blob, err := encodeRun(in, testPayloadCeiling)
	if err != nil {
		t.Fatalf("encodeRun() error = %v", err)
	}
	out, err := decodeRun(blob, testPayloadCeiling)
	if err != nil {
		t.Fatalf("decodeRun() error = %v", err)
	}
	if len(out.Components) != 1 {
		t.Fatalf("decoded Components = %d rows, want 1", len(out.Components))
	}

	outV := reflect.ValueOf(&out.Components[0]).Elem()
	for i := range rt.NumField() {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		want := rv.Field(i).Interface()
		got := outV.Field(i).Interface()
		if !reflect.DeepEqual(want, got) {
			t.Errorf("ComponentState.%s round-tripped as %#v, want %#v -- envelope.go does not carry it",
				f.Name, got, want)
		}
	}
}
