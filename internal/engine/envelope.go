package engine

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// envelopeVersion is the persisted schema version. It exists so a future
// format change is safe to roll out against a ConfigMap written by a previous
// image: an unrecognized version is refused rather than partially decoded.
const envelopeVersion = 1

// ephemeralArtifacts are dropped on encode. bundle.path points into the
// chart's emptyDir, which does not survive a restart -- persisting it would
// hand a recovered Apply a path to a directory that no longer exists, which
// is strictly worse than the key being absent.
var ephemeralArtifacts = map[string]bool{"bundle.path": true}

// envelope is the persisted projection of a Run. It exists rather than
// reusing the API's json tags because Run.Artifacts is json:"-" -- that tag
// is load-bearing (it keeps snapshot.yaml out of HTTP responses) and must
// stay, so the store carries artifacts deliberately instead.
type envelope struct {
	Version    int               `json:"version"`
	ID         string            `json:"id"`
	State      State             `json:"state"`
	Phase      Phase             `json:"phase"`
	Decisions  map[string]string `json:"decisions,omitempty"`
	Pending    []string          `json:"pending,omitempty"`
	Components []ComponentState  `json:"components,omitempty"`
	StepIndex  int               `json:"stepIndex"`
	Err        string            `json:"error,omitempty"`
	StartedAt  time.Time         `json:"startedAt"`
	UpdatedAt  time.Time         `json:"updatedAt"`
	Artifacts  map[string][]byte `json:"artifacts,omitempty"`
	// Truncated names the artifacts encodeRun dropped to fit maxPayload, so
	// the record says what is missing rather than looking complete. Added
	// without bumping envelopeVersion on purpose: it is optional on both
	// sides, so a record written by this build still decodes in a build
	// rolled back to the previous image, and bumping the version would turn
	// a rollback into an unreadable-record degradation for no gain.
	Truncated []string `json:"truncated,omitempty"`
	// Workload is the same optional-on-both-sides addition as Truncated:
	// a record written before this field existed has no "workload" key,
	// gunzipJSON leaves it as the zero value, and that is a correct decode,
	// not a degraded one -- so this does not bump envelopeVersion either.
	//
	// omitzero, not omitempty: see Run.Workload's comment. omitempty would
	// write the zero-value struct into every record that never went active.
	Workload Workload `json:"workload,omitzero"`
	// CleanupUnconfirmed is the same optional-on-both-sides addition as
	// Truncated and Workload: a record written before this field existed has
	// no "cleanupUnconfirmed" key, gunzipJSON leaves it false, and that is a
	// correct decode for every record that predates Ruling 12 (nothing
	// before this field could have set it true), not a degraded one -- so
	// this does not bump envelopeVersion either.
	//
	// Fix round 2's N1: this envelope is a hand-maintained projection, not
	// Run's own json tags (see the type's doc comment) -- Run.CleanupUnconfirmed
	// existed for a full fix round without a producer here, so a pod restart
	// silently dropped Ruling 12's guard while carrying the OLD, now-removed
	// Run.Err text it replaced. decodeRun's caller (Recover) leaves a
	// recovered StateFailed run's State exactly as stored (recover.go only
	// touches isLive/StateIdle states), so this field surviving the round
	// trip is what makes the guard survive a restart at all.
	CleanupUnconfirmed bool `json:"cleanupUnconfirmed,omitempty"`
	// Ownership is the same optional-on-both-sides addition as Truncated,
	// Workload and CleanupUnconfirmed, and does not bump envelopeVersion for
	// the same reason. The decode of a record written before this field
	// existed is not merely tolerable, it is CORRECT: the zero value means
	// "no ownership evidence", and internal/teardown reads that as "prove
	// nothing, remove nothing", which is the fail-closed direction. A
	// version bump would instead make every pre-existing record unreadable,
	// turning a run that is safe to Reset conservatively into one no
	// operator action can reach.
	//
	// omitzero, not omitempty: see Run.Ownership's comment.
	Ownership Ownership `json:"ownership,omitzero"`
	// Residue is the same optional-on-both-sides addition, and the one that
	// matters most for a rollback: it carries hasIncompleteTeardown's guard,
	// so a record whose Reset failed keeps refusing Start, Retry and Discard
	// across a pod restart. That is the CleanupUnconfirmed lesson (fix round
	// 2's N1) applied before the fact rather than after it -- and
	// envelope_test.go's parity test is what will catch it if a later field
	// is added here without a producer.
	//
	// omitzero, not omitempty: see Run.Residue's comment.
	Residue Residue `json:"residue,omitzero"`
}

func gzipJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// gunzipJSON decompresses blob and unmarshals it into v. maxDecompressed
// bounds the expansion -- see decodeRun for why that bound exists and how it
// is sized.
func gunzipJSON(blob []byte, v any, maxDecompressed int) error {
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()
	// LimitReader + 1 so a record exactly at the bound is distinguishable
	// from one that exceeds it.
	raw, err := io.ReadAll(io.LimitReader(zr, int64(maxDecompressed)+1))
	if err != nil {
		return err
	}
	if len(raw) > maxDecompressed {
		return fmt.Errorf("decompressed record exceeds %d bytes", maxDecompressed)
	}
	return json.Unmarshal(raw, v)
}

// encodeRun serializes r for persistence. It never mutates the caller's run.
//
// maxPayload bounds the encoded record. It is a parameter rather than a
// constant because it describes the STORE, not the run: a ConfigMap is capped
// near 1 MiB by Kubernetes, a file is not. A record over the ceiling sheds
// artifacts, largest first, until it fits, and names what it dropped in
// Truncated.
//
// Largest-first minimizes HOW MUCH IS LOST -- the fewest artifacts shed to get
// under the cap -- and nothing more than that. It does NOT preserve
// retryability, and an earlier version of this comment claimed it did, on the
// reasoning that recovery rewinds to Bundle and Bundle reads only the small
// recipe.json. That is wrong: internal/steps/bundle.go reads recipe.json AND
// decodeSnapshot(run.Artifacts["snapshot.yaml"]), so wherever shedding fires
// at all, snapshot.yaml is the first thing to go and the rewound retry fails
// immediately at decodeSnapshot. Truncation is a one-way door for this run.
//
// What survives is the state machine itself: ID, state, phase, StepIndex,
// decisions, and the component projection. That is enough to recover the run
// as a record an operator can see and discard, which is the point -- a worse
// outcome than a complete record, and a far better one than a console with no
// reachable action at all. Run.Truncated carries the loss out of the store so
// the console can say so instead of offering a retry that cannot work.
//
// It still fails closed when there is nothing left to shed: a record whose
// decisions and component rows alone exceed the limit is not a large
// cluster, it is a bug, and silently persisting a record with fields
// dropped from the state machine would be worse than refusing.
func encodeRun(r *Run, maxPayload int) ([]byte, error) {
	env := envelope{
		Version:            envelopeVersion,
		ID:                 r.ID,
		State:              r.State,
		Phase:              r.Phase,
		Decisions:          r.Decisions,
		Pending:            r.Pending,
		Components:         r.Components,
		StepIndex:          r.StepIndex,
		Err:                r.Err,
		StartedAt:          r.StartedAt,
		UpdatedAt:          r.UpdatedAt,
		Workload:           r.Workload,
		CleanupUnconfirmed: r.CleanupUnconfirmed,
		Ownership:          r.Ownership,
		Residue:            r.Residue,
		Artifacts:          make(map[string][]byte, len(r.Artifacts)),
		// Carried forward, not recomputed. A run recovered from a truncated
		// record no longer HAS the shed artifact, so re-encoding it would fit
		// on the first try and produce a record claiming completeness while
		// still missing everything the first truncation dropped. A shed key
		// is absent from r.Artifacts by definition, so the loop below can
		// never append a duplicate.
		Truncated: append([]string(nil), r.Truncated...),
	}
	for k, v := range r.Artifacts {
		if ephemeralArtifacts[k] {
			continue
		}
		env.Artifacts[k] = v
	}
	blob, err := gzipJSON(env)
	if err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "encoding run failed", err)
	}
	if len(blob) <= maxPayload {
		return blob, nil
	}

	shedBytes := 0
	for len(env.Artifacts) > 0 && len(blob) > maxPayload {
		key := largestArtifact(env.Artifacts)
		shedBytes += len(env.Artifacts[key])
		delete(env.Artifacts, key)
		env.Truncated = append(env.Truncated, key)
		if blob, err = gzipJSON(env); err != nil {
			return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInternal, "encoding run failed", err)
		}
	}
	if len(blob) > maxPayload {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("run state too large to checkpoint: %d bytes compressed exceeds the payload ceiling of %d bytes", len(blob), maxPayload))
	}

	slog.Warn("run checkpoint exceeded its payload ceiling; artifacts were dropped so the record could still be written. "+
		"Durability is degraded: this run is still recoverable as a state machine, but a retry that reads one of the dropped artifacts will fail and the operator will have to discard and start over",
		"run", r.ID, "dropped", env.Truncated, "droppedBytes", shedBytes,
		"compressedBytes", len(blob), "limit", maxPayload)
	return blob, nil
}

// largestArtifact returns the key of the biggest artifact. Ties break on the
// key name so the same run always sheds in the same order -- a checkpoint
// that dropped a different artifact on each save would make a recovered
// record's contents depend on map iteration order.
func largestArtifact(artifacts map[string][]byte) string {
	best := ""
	found := false
	for k, v := range artifacts {
		switch {
		case !found, len(v) > len(artifacts[best]):
			best, found = k, true
		case len(v) == len(artifacts[best]) && k < best:
			best = k
		}
	}
	return best
}

// decompressMultiplier scales maxPayload into decodeRun's decompression-bomb
// guard. It is not sized to reproduce any particular old relationship
// exactly -- an earlier version of this comment claimed the ×10 it replaced
// reproduced the ConfigMap store's original 800 KiB to 8 MiB ratio "exactly",
// and that arithmetic was wrong (819,200 × 10 = 8,192,000, which is narrower
// than 8 << 20 = 8,388,608, so decodeRun would have newly rejected a
// decompressed record between roughly 7.8 and 8 MiB that the old,
// unparameterized bound accepted). The one property that matters is that
// parameterizing the ceiling must never narrow what decodeRun accepts:
// cmPayloadCeiling * decompressMultiplier (9,011,200) is deliberately kept
// wider than the ConfigMap store's original 8 << 20, with headroom rather
// than a value pinned to the boundary.
const decompressMultiplier = 11

// decodeRun deserializes a record written by encodeRun. An unrecognized
// version is refused rather than partially decoded: guessing at a format
// written by a different image is how a newer record gets silently
// downgraded.
//
// maxPayload is the same ceiling encodeRun was given. Decompression is
// bounded at maxPayload * decompressMultiplier: a decompression-bomb guard
// scaled to whatever ceiling the store supplies, rather than a fixed number,
// and deliberately never narrower than the ConfigMap store's original 8 MiB
// -- so parameterizing the ceiling rejects nothing decodeRun previously
// accepted. That bound is a property of this decoder, not of where the bytes
// were kept.
func decodeRun(blob []byte, maxPayload int) (*Run, error) {
	maxDecompressed := maxPayload * decompressMultiplier

	var env envelope
	if err := gunzipJSON(blob, &env, maxDecompressed); err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "decoding run failed", err)
	}
	if env.Version != envelopeVersion {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported run schema version %d (this build writes %d)", env.Version, envelopeVersion))
	}
	r := &Run{
		ID:                 env.ID,
		State:              env.State,
		Phase:              env.Phase,
		Decisions:          env.Decisions,
		Pending:            env.Pending,
		Components:         env.Components,
		StepIndex:          env.StepIndex,
		Err:                env.Err,
		StartedAt:          env.StartedAt,
		UpdatedAt:          env.UpdatedAt,
		Artifacts:          env.Artifacts,
		Truncated:          env.Truncated,
		Workload:           env.Workload,
		CleanupUnconfirmed: env.CleanupUnconfirmed,
		Ownership:          env.Ownership,
		Residue:            env.Residue,
	}
	if r.Decisions == nil {
		r.Decisions = map[string]string{}
	}
	if r.Artifacts == nil {
		r.Artifacts = map[string][]byte{}
	}
	// The write side already warned once, in the process that shed them. This
	// is the read side saying the same thing to whoever is looking at the
	// startup log of the process that has to live with the consequence.
	if len(env.Truncated) > 0 {
		slog.Warn("run checkpoint was written truncated; these artifacts are absent, so a retry that reads one of them will fail and the run has to be discarded instead",
			"run", env.ID, "dropped", env.Truncated)
	}
	return r, nil
}
