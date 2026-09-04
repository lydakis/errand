package setup

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"path"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

// renderConfig writes only the keys setup decided, with comments, so the
// file reads as documentation of what this runner does.
func renderConfig(c ConfigChoice) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# errand runner configuration written by `errand setup`.\n")
	fmt.Fprintf(&b, "# Edit freely; setup never rewrites this file without --force.\n\n")
	fmt.Fprintf(&b, "listen = %q\n", c.Listen)
	fmt.Fprintf(&b, "max_jobs = %d\n", c.MaxJobs)
	fmt.Fprintf(&b, "\n# Tailnet logins granted full runner access (the runner's owner by default).\n")
	fmt.Fprintf(&b, "allow_users = [")
	for i, u := range c.AllowUsers {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%q", u)
	}
	fmt.Fprintf(&b, "]\n\n")
	switch {
	case c.TailscaledSocket != "":
		fmt.Fprintf(&b, "# How this runner reaches tailscaled to identify callers (discovered by setup).\n")
		fmt.Fprintf(&b, "tailscaled_socket = %q\n", c.TailscaledSocket)
	case c.TailscaleCLI != "":
		fmt.Fprintf(&b, "# The standalone Tailscale app exposes no socket; the CLI identifies callers.\n")
		fmt.Fprintf(&b, "# Capabilities cannot be destination-scoped this way, so allow_users authorizes.\n")
		fmt.Fprintf(&b, "tailscale_cli = %q\n", c.TailscaleCLI)
	}
	return b.String()
}

// renderSystemdUnit is the Linux user service. Journald keeps the logs;
// Restart=on-failure and linger (enabled separately) keep it up.
func renderSystemdUnit(executable, configPath, runnerPath string) string {
	return fmt.Sprintf(`[Unit]
Description=errand runner (remote job daemon)
Documentation=https://github.com/lydakis/errand
After=network-online.target tailscaled.service
Wants=network-online.target

[Service]
Type=simple
Environment=%s
ExecStart=%s serve --config %s
Restart=on-failure
RestartSec=5s
TimeoutStopSec=30s

[Install]
WantedBy=default.target
`, systemdQuote("PATH="+runnerPath), systemdQuote(executable), systemdQuote(configPath))
}

func systemdQuote(value string) string {
	value = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(value)
	return `"` + value + `"`
}

// renderLaunchAgent is the macOS user agent. KeepAlive restarts it after a
// crash; RunAtLoad starts it at login.
func renderLaunchAgent(label, executable, configPath, logPath, runnerPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>%s</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>serve</string>
		<string>--config</string>
		<string>%s</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>%s</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>%s</string>
	<key>StandardErrorPath</key>
	<string>%s</string>
</dict>
</plist>
`, xmlText(label), xmlText(executable), xmlText(configPath), xmlText(runnerPath), xmlText(logPath), xmlText(logPath))
}

func servicePath(raw string) string {
	defaults := []string{"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin"}
	seen := make(map[string]bool)
	entries := make([]string, 0, len(strings.Split(raw, ":"))+len(defaults))
	add := func(entry string) {
		if !strings.HasPrefix(entry, "/") || strings.ContainsAny(entry, "\x00\r\n") {
			return
		}
		entry = path.Clean(entry)
		if !seen[entry] {
			seen[entry] = true
			entries = append(entries, entry)
		}
	}
	for _, entry := range strings.Split(raw, ":") {
		add(entry)
	}
	for _, entry := range defaults {
		add(entry)
	}
	return strings.Join(entries, ":")
}

func xmlText(value string) string {
	var escaped bytes.Buffer
	_ = xml.EscapeText(&escaped, []byte(value))
	return escaped.String()
}

// RenderACL prints the tailnet grant for capability-based authorization.
func RenderACL(capability string, port int) string {
	return fmt.Sprintf(`{
  "tagOwners": { "tag:errand-runner": ["autogroup:admin"] },
  "grants": [{
    "src": ["autogroup:member"],
    "dst": ["tag:errand-runner"],
    "ip":  ["tcp:%d"],
    "app": {
      %q: [
        { "actions": [%q, %q, %q, %q, %q, %q] }
      ]
    }
  }]
}
`, port, capability,
		proto.ActionSubmit, proto.ActionReadOwn, proto.ActionKillOwn,
		proto.ActionForwardOwn, proto.ActionCaches, proto.ActionGCJobs,
	)
}
