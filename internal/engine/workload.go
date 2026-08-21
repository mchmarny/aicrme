package engine

// Workload identifies the reference workload a Prove run left running, so
// the console can name it after a restart. It is a display convenience, not
// an identity: correctness must not depend on it.
//
// Terminal saves are best-effort and the run store can degrade to memory
// (see cmstore.go), so a console that could only find its workload by
// reading this field would lose track of it exactly when the record was
// lost, while the workload kept holding GPUs. That is why internal/prove
// makes labels the primary key and derives the workload name from the run
// ID (prove.WorkloadName): Task 8's reconciliation finds workloads by label,
// and only falls back to this field as a hint.
type Workload struct {
	Namespace string `json:"namespace"`
	Kind      string `json:"kind"`
	Name      string `json:"name"`
}
