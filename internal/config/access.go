package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/BurntSushi/toml"
	"github.com/lydakis/errand/internal/proto"
)

// AccessPolicy describes saved runner configuration, not live authorization.
type AccessPolicy struct {
	Path       string   `json:"path" toml:"-"`
	AllowUsers []string `json:"allow_users" toml:"allow_users"`
	DenyUsers  []string `json:"deny_users" toml:"deny_users"`
	Capability string   `json:"capability" toml:"capability"`
	Listen     string   `json:"listen" toml:"listen"`
}

type AccessChange struct {
	Field   string   `json:"field"`
	Path    string   `json:"path"`
	Before  []string `json:"before"`
	After   []string `json:"after"`
	Changed bool     `json:"changed"`
	Written bool     `json:"written"`
}

func ReadAccess(path string) (AccessPolicy, error) {
	policy, _, _, err := readAccessDocument(path)
	return policy, err
}

func readAccessDocument(path string) (AccessPolicy, map[string]any, []byte, error) {
	var policy AccessPolicy
	if path == "" {
		// Setup installs the service with this explicit path, independently
		// of the client's XDG_CONFIG_HOME.
		home, err := userHomeDir()
		if err != nil {
			return policy, nil, nil, err
		}
		path = filepath.Join(home, ".config", "errand", "errandd.toml")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return policy, nil, nil, err
	}
	info, err := os.Lstat(abs)
	if os.IsNotExist(err) {
		return policy, nil, nil, fmt.Errorf("runner config %s is missing; run errand setup first", abs)
	}
	if err != nil {
		return policy, nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return policy, nil, nil, fmt.Errorf("runner config %s must be a regular file, not a symlink or directory", abs)
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return policy, nil, nil, err
	}
	var document map[string]any
	if _, err := toml.Decode(string(raw), &document); err != nil {
		return policy, nil, nil, fmt.Errorf("%s: %w", abs, err)
	}
	if _, err := toml.Decode(string(raw), &policy); err != nil {
		return policy, nil, nil, fmt.Errorf("%s: %w", abs, err)
	}
	policy.Path = abs
	if policy.AllowUsers == nil {
		policy.AllowUsers = []string{}
	}
	if policy.DenyUsers == nil {
		policy.DenyUsers = []string{}
	}
	if policy.Capability == "" {
		policy.Capability = proto.DefaultCapability
	}
	if policy.Listen == "" {
		policy.Listen = "tailnet:7443"
	}
	return policy, document, raw, nil
}

// ChangeAccess updates only the allow_users value. Other TOML values, including
// unknown settings, survive re-encoding; comments and formatting do not. A
// preview or no-op leaves the file byte-for-byte unchanged. No service is touched.
func ChangeAccess(path, login string, add, dryRun bool) (AccessChange, error) {
	return changeAccessList(path, login, "allow_users", add, dryRun)
}

// ChangeDeniedAccess edits deny_users with the same persistence contract as
// ChangeAccess. It preserves grants; removing a denial restores their effect.
func ChangeDeniedAccess(path, login string, deny, dryRun bool) (AccessChange, error) {
	return changeAccessList(path, login, "deny_users", deny, dryRun)
}

func changeAccessList(path, login, field string, add, dryRun bool) (AccessChange, error) {
	var result AccessChange
	if login == "" || strings.ContainsAny(login, "*?") || strings.IndexFunc(login, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return result, fmt.Errorf("login must be an exact non-empty tailnet login without whitespace or wildcards")
	}
	policy, document, raw, err := readAccessDocument(path)
	if err != nil {
		return result, err
	}
	result.Path = policy.Path
	result.Field = field
	users := policy.AllowUsers
	if field == "deny_users" {
		users = policy.DenyUsers
	}
	result.Before = slices.Clone(users)
	result.After = slices.Clone(users)
	if add {
		if !slices.Contains(result.After, login) {
			result.After = append(result.After, login)
		}
	} else {
		result.After = slices.DeleteFunc(result.After, func(existing string) bool { return existing == login })
	}
	result.Changed = !slices.Equal(result.Before, result.After)
	if dryRun || !result.Changed {
		return result, nil
	}
	document[field] = result.After
	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(document); err != nil {
		return result, err
	}
	// Refuse observable intervening edits, including replacement by a symlink.
	info, err := os.Lstat(policy.Path)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, fmt.Errorf("runner config changed while editing; retry")
	}
	current, err := os.ReadFile(policy.Path)
	if err != nil {
		return result, err
	}
	if !bytes.Equal(current, raw) {
		return result, fmt.Errorf("runner config changed while editing; retry")
	}
	if err := writeFile(policy.Path, encoded.String()); err != nil {
		return result, err
	}
	result.Written = true
	return result, nil
}
