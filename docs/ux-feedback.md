# UX feedback

A running list of UX observations from **actually running the demo**, kept so they are not lost
between sessions. Nothing here is scheduled — capture first, decide later.

Each entry records what was observed, where in the code it lives, and anything that constrains
the fix. Entries are removed when they ship, not when they are agreed with.

---

## 1. The event stream appends, so the newest event is off-screen

**Observed:** 2026-08-19, Mark, first local `make demo` run, during Apply.

The right-hand event stream appends newest-last. During a 14-action install it grows past the
viewport, so the operator has to scroll to see what is happening *now* — during precisely the
five minutes the demo is meant to be watched.

**Proposed:** prepend, so the newest event is always at the top and no scrolling is required to
follow a live run.

**Where:** `web/src/components/Timeline.tsx:15` (`events.map(...)`), and its test at
`web/src/components/Timeline.test.tsx`.

**Worth thinking about before changing it:**
- The bus replays from a ring buffer on reconnect, so ordering has to hold for a late-joining
  tab too, not just a live one.
- Reversing display order affects how a *multi-line* event reads — several events in the
  screenshot wrap to two or three lines, and newest-first means a wrapped event's continuation
  sits below its own timestamp while the previous event sits below that. Worth checking it still
  scans.
- The timeline currently reads chronologically, which matches how the install narrates itself
  ("phase started" → "phase complete"). Newest-first inverts that for a reader catching up.
  Both orderings are defensible; the live-watching case is the one the demo optimises for.

---

## Observed in the same screenshot, not raised as feedback

Recorded only so they are not re-discovered as surprises. Neither is a request.

- The Discover gap list renders exactly as intended on a simulated cluster — "No GPU driver
  installed", "No device plugin", "No GPU-aware scheduler", and an explicit "this is a simulated
  cluster" line. That is the copy honesty the design asked for, working.
- 2b-iii's per-row condition renders correctly on a real install: `RolloutProgress
  node-feature-discovery/nfd-node-feature-discovery-worker 0/3 nodes ready (cluster activity
  while nfd installs)`. First confirmation from a human-driven run that the attributed condition
  lands on the right row with the temporal copy intact.
