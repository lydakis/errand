package workspace

import "fmt"

// Profile is a named set of run preferences shared by personal and workspace
// configuration. It cannot change snapshot boundaries or define transports.
type Profile struct {
	Caches      Caches      `toml:"caches"`
	Artifacts   Artifacts   `toml:"artifacts"`
	Session     Session     `toml:"session"`
	Environment Environment `toml:"env,omitempty"`
	Run         struct {
		Peer    *string `toml:"peer,omitempty"`
		Workdir *string `toml:"workdir,omitempty"`
	} `toml:"run,omitempty"`
	Changes struct {
		ApplyOnSuccess *bool `toml:"apply_on_success,omitempty"`
	} `toml:"changes,omitempty"`
}

// UnmarshalTOML rejects unsupported profile settings instead of silently
// ignoring a misspelled preference or promising unimplemented behavior.
func (p *Profile) UnmarshalTOML(value any) error {
	table, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("profile must be a table")
	}
	*p = Profile{}
	for section, raw := range table {
		if section == "caches" {
			if err := p.Caches.UnmarshalTOML(raw); err != nil {
				return err
			}
			continue
		}
		if section == "artifacts" {
			if err := p.Artifacts.UnmarshalTOML(raw); err != nil {
				return err
			}
			continue
		}
		if section == "session" {
			if err := p.Session.UnmarshalTOML(raw); err != nil {
				return err
			}
			continue
		}
		if section == "env" {
			if err := p.Environment.UnmarshalTOML(raw); err != nil {
				return err
			}
			continue
		}
		if section != "run" && section != "changes" {
			return fmt.Errorf("unsupported profile section %q", section)
		}
		fields, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("profile.%s must be a table", section)
		}
		for key, raw := range fields {
			switch section + "." + key {
			case "run.peer", "run.workdir":
				value, ok := raw.(string)
				if !ok {
					return fmt.Errorf("profile.%s.%s must be a string", section, key)
				}
				if key == "peer" {
					p.Run.Peer = &value
				} else {
					p.Run.Workdir = &value
				}
			case "changes.apply_on_success":
				value, ok := raw.(bool)
				if !ok {
					return fmt.Errorf("profile.changes.apply_on_success must be a boolean")
				}
				p.Changes.ApplyOnSuccess = &value
			default:
				return fmt.Errorf("unsupported profile setting %q", section+"."+key)
			}
		}
	}
	return nil
}
