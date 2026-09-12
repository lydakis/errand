package config

import "fmt"

// PrepareExecution applies persistent-job rules to resolved preferences. Run
// submission and inspection call this; workspace creation deliberately does not.
// Remote defaults remain unknown until the client loads the selected workspace.
func (e *EffectiveRun) PrepareExecution(includeAll bool) error {
	if e.Workspace == "" {
		return nil
	}
	if e.NoSnapshot || includeAll {
		return fmt.Errorf("persistent workspace runs cannot use --no-snapshot or --include-all")
	}
	if e.Where != "" {
		return fmt.Errorf("persistent workspace requires a pinned peer; use --on instead of where")
	}
	if e.Sources == nil {
		e.Sources = make(map[string]string)
	}
	source := fmt.Sprintf("persistent workspace %q: creation defaults (resolved on runner)", e.Workspace)
	if !e.CachesOverride {
		e.Caches = nil
		e.Sources["caches"] = source
	}
	if !e.ArtifactsOverride {
		e.Artifacts = nil
		e.Sources["artifacts"] = source
	}
	return nil
}
