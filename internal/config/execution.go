package config

import "fmt"

// PrepareExecution applies persistent-job rules and loads the job environment.
// Run, config, and doctor call this; source push and workspace creation do not.
// Remote defaults remain unknown until the client loads the selected workspace.
func (e *EffectiveRun) PrepareExecution(includeAll bool) error {
	if e.Workspace != "" {
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
	}
	var err error
	e.Environment, err = resolveEnvironment(e.environmentLayers...)
	return err
}
