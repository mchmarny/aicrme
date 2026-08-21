package engine

// ActiveStep is implemented by a Step that leaves something running in the
// cluster after Run returns. The engine finishes such a run at StateActive
// rather than StateDone, so the console keeps tracking what the step left
// behind and the operator retains a way to stop it.
//
// Deliberately an optional interface rather than a method on Step: Discover,
// Recommend, Bundle and Apply leave nothing running, and none of them should
// have to say so.
type ActiveStep interface {
	LeavesWorkloadRunning() bool
}

// isActive reports whether step wants its run to end at StateActive. Only the
// final step is consulted -- an ActiveStep followed by other steps has had
// its work superseded by the time the run ends.
func isActive(step Step) bool {
	as, ok := step.(ActiveStep)
	return ok && as.LeavesWorkloadRunning()
}
