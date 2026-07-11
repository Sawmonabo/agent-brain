# Backlog: engine-level withholding on project-identity drift

Doctor's `project-identity` check (advisory) detects the drift; it does not
stop it — a headless machine never runs the battery and keeps mirroring
bidirectionally into a folder the fleet reassigned (cross-project
contamination) until someone runs `doctor` or opens the dashboard.

The stronger guarantee is engine-side: verify `unit.ProjectID` against the
shared registry POST-INTEGRATE each cycle (the reassignment arrives in the
same fetch) and withhold that unit's mirror-in/mirror-out as a new degrade
class. That is a spec §4/§5 semantics change (a per-cycle registry read and
new degrade vocabulary) and needs its own ADR + design pass — do not bolt
it onto another wave.
