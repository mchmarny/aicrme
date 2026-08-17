package engine

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"time"

	aicrerrors "github.com/NVIDIA/aicr/pkg/errors"
)

// envelopeVersion is the persisted schema version. It exists so a future
// format change is safe to roll out against a ConfigMap written by a previous
// image: an unrecognized version is refused rather than partially decoded.
const envelopeVersion = 1

// maxPayload bounds the encoded record. Kubernetes caps a ConfigMap at
// roughly 1MiB; failing at 800KiB with a named error beats letting the API
// server reject an oversized object with something opaque, and leaves room
// for the object's own metadata.
const maxPayload = 800 << 10

// maxDecompressed bounds what decodeRun will expand. A small stored payload
// can inflate without limit, and the pod runs under a 512Mi cap, so an
// unbounded reader turns a malformed record into an OOM kill instead of an
// error.
const maxDecompressed = 8 << 20

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

func gunzipJSON(blob []byte, v any) error {
	zr, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()
	// LimitReader + 1 so a record exactly at the bound is distinguishable
	// from one that exceeds it.
	raw, err := io.ReadAll(io.LimitReader(zr, maxDecompressed+1))
	if err != nil {
		return err
	}
	if len(raw) > maxDecompressed {
		return fmt.Errorf("decompressed record exceeds %d bytes", maxDecompressed)
	}
	return json.Unmarshal(raw, v)
}

// encodeRun projects a Run into a compressed envelope. It never mutates the
// caller's run.
func encodeRun(r *Run) ([]byte, error) {
	env := envelope{
		Version:    envelopeVersion,
		ID:         r.ID,
		State:      r.State,
		Phase:      r.Phase,
		Decisions:  r.Decisions,
		Pending:    r.Pending,
		Components: r.Components,
		StepIndex:  r.StepIndex,
		Err:        r.Err,
		StartedAt:  r.StartedAt,
		UpdatedAt:  r.UpdatedAt,
		Artifacts:  make(map[string][]byte, len(r.Artifacts)),
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
	if len(blob) > maxPayload {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("run state too large to checkpoint: %d bytes compressed, limit %d", len(blob), maxPayload))
	}
	return blob, nil
}

// decodeRun reverses encodeRun. An unrecognized version is refused rather
// than partially decoded: guessing at a format written by a different image
// is how a newer record gets silently downgraded.
func decodeRun(blob []byte) (*Run, error) {
	var env envelope
	if err := gunzipJSON(blob, &env); err != nil {
		return nil, aicrerrors.Wrap(aicrerrors.ErrCodeInvalidRequest, "decoding run failed", err)
	}
	if env.Version != envelopeVersion {
		return nil, aicrerrors.New(aicrerrors.ErrCodeInvalidRequest,
			fmt.Sprintf("unsupported run schema version %d (this build writes %d)", env.Version, envelopeVersion))
	}
	r := &Run{
		ID:         env.ID,
		State:      env.State,
		Phase:      env.Phase,
		Decisions:  env.Decisions,
		Pending:    env.Pending,
		Components: env.Components,
		StepIndex:  env.StepIndex,
		Err:        env.Err,
		StartedAt:  env.StartedAt,
		UpdatedAt:  env.UpdatedAt,
		Artifacts:  env.Artifacts,
	}
	if r.Decisions == nil {
		r.Decisions = map[string]string{}
	}
	if r.Artifacts == nil {
		r.Artifacts = map[string][]byte{}
	}
	return r, nil
}
