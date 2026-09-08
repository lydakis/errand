package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/tailnet"
)

// savedConfig retains the original bytes for reconciliation and the final
// concurrent-edit check. A forced repair may have bytes but no decoded daemon.
type savedConfig struct {
	exists bool
	raw    []byte
	daemon *config.Daemon
}

type transportPlan struct {
	choice            ConfigChoice
	tailnetConfigured bool
}

type configPlan struct {
	rendered  string
	changed   bool
	effective config.Daemon
}

// Discovery can inspect Tailscale but cannot write files or restart services.
type transportSystem interface {
	Discover(socket, cli string) (tailnet.Provider, error)
}

func resolveTransport(ctx context.Context, opts Options, sys transportSystem, saved savedConfig, r *Report) (transportPlan, error) {
	// The configuration is the source of truth. Flags edit the preference;
	// availability only determines whether a requested tailnet can be enabled.
	var err error
	mode := config.TransportBoth
	if opts.Transport != "" {
		mode, err = (config.Daemon{Transport: opts.Transport}).TransportMode()
	} else if saved.daemon != nil {
		mode, err = saved.daemon.TransportMode()
	}
	if err != nil {
		return transportPlan{}, err
	}
	socketOnly := mode == config.TransportSSH || mode == config.TransportLocal
	explicitTailnet := opts.Socket != "" || opts.CLI != "" || len(opts.AllowUsers) != 0
	if socketOnly && explicitTailnet {
		label := mode
		if mode == config.TransportSSH {
			label = "SSH"
		}
		return transportPlan{}, fmt.Errorf("%s-only setup cannot use --tailscaled-socket, --tailscale-cli, or --allow-user", label)
	}
	// Retained listeners carry an existing policy even while socket-only.
	// Tailscale-only also enables the default listener when listen is "none".
	tailnetConfigured := saved.daemon != nil && !strings.EqualFold(strings.TrimSpace(saved.daemon.Listen), config.DisabledListener)
	if saved.daemon != nil {
		oldMode, modeErr := saved.daemon.TransportMode()
		// An unknown saved mode cannot establish first-time activation;
		// preserve its implicit authorization policy when repairing it.
		tailnetConfigured = tailnetConfigured || oldMode == config.TransportTailscale || modeErr != nil
	}
	var provider tailnet.Provider
	if !socketOnly {
		providerSocket, providerCLI := opts.Socket, opts.CLI
		if providerSocket == "" && providerCLI == "" && saved.daemon != nil {
			providerSocket = saved.daemon.TailscaledSocket
			providerCLI = saved.daemon.TailscaleCLI
		}
		provider, err = sys.Discover(providerSocket, providerCLI)
		var self tailnet.Self
		if err == nil {
			identityCtx, cancel := context.WithTimeout(ctx, probeTimeout)
			self, err = provider.Self(identityCtx)
			cancel()
		}
		if err != nil {
			if ctx.Err() != nil {
				return transportPlan{}, ctx.Err()
			}
			if tailnetConfigured || mode == config.TransportTailscale || explicitTailnet || saved.exists && saved.daemon == nil {
				return transportPlan{}, err
			}
			socketOnly = true
			r.step("tailnet", "unavailable: "+err.Error()+"; connect Tailscale and rerun errand setup to enable tailnet access", false)
		} else {
			if !tailnet.SupportsDestinationScopedWhoIs(self.Version) {
				return transportPlan{}, fmt.Errorf("tailscaled %q is too old: errand requires 1.100 or newer", self.Version)
			}
			r.Provider = provider.Name()
			r.Self = self
			r.step("tailnet", fmt.Sprintf("%s via %s; this node is %s, owned by %s",
				self.Version, provider.Name(), self.DNSName, self.Login), false)
		}
	}
	if mode != config.TransportTailscale && mode != config.TransportLocal {
		r.step("ssh", "SSH bridge enabled; enable SSH login to this account on the runner if needed", false)
	}

	// Choose defaults for a new config or the requested transport transition.
	choice := ConfigChoice{Transport: mode, Listen: fmt.Sprintf("tailnet:%d", DefaultPort), MaxJobs: opts.MaxJobs}
	if choice.MaxJobs <= 0 {
		choice.MaxJobs = 1
	}
	if socketOnly {
		choice.Listen = config.DisabledListener
	} else {
		if saved.daemon != nil && !opts.Force && saved.daemon.Listen != "" && !strings.EqualFold(strings.TrimSpace(saved.daemon.Listen), config.DisabledListener) {
			choice.Listen = saved.daemon.Listen
		}
		choice.AllowUsers = uniqueSorted(append([]string{r.Self.Login}, opts.AllowUsers...))
		switch {
		case strings.HasPrefix(provider.Name(), "localapi:"):
			choice.TailscaledSocket = strings.TrimPrefix(provider.Name(), "localapi:")
		case strings.HasPrefix(provider.Name(), "cli:"):
			choice.TailscaleCLI = strings.TrimPrefix(provider.Name(), "cli:")
		}
	}

	return transportPlan{choice: choice, tailnetConfigured: tailnetConfigured}, nil
}

// reconcileConfig is a pure decision: preserve the saved policy and unrelated
// settings, producing the bytes to write and the effective daemon configuration.
// Run validates that configuration before acquiring a restart lease or writing.
func reconcileConfig(opts Options, saved savedConfig, transport transportPlan) (configPlan, error) {
	choice := transport.choice
	mode := choice.Transport
	rendered := renderConfig(choice)
	configChanged := !saved.exists || opts.Force
	effective := config.Daemon{Transport: mode, Listen: choice.Listen, AllowUsers: choice.AllowUsers,
		TailscaledSocket: choice.TailscaledSocket, TailscaleCLI: choice.TailscaleCLI, MaxJobs: choice.MaxJobs, MaxQueued: 8}
	if saved.daemon != nil && !opts.Force {
		effective = *saved.daemon
		oldMode, modeErr := effective.TransportMode()
		previous := effective
		if modeErr != nil {
			// A valid explicit override permits repair. Use the saved listener
			// as-is to retain its address without interpreting the invalid mode.
			previous.Transport = ""
		}
		if err := previous.NormalizeTransport(); err != nil {
			return configPlan{}, err
		}
		oldListen := previous.Listen
		if !strings.EqualFold(strings.TrimSpace(oldListen), choice.Listen) || oldMode != mode || opts.Transport != "" && effective.Transport != mode {
			var document map[string]any
			if err := toml.Unmarshal(saved.raw, &document); err != nil {
				return configPlan{}, err
			}
			document["transport"] = mode
			// Disabling a transport need not discard a custom listener address.
			if mode != config.TransportSSH && mode != config.TransportLocal {
				document["listen"] = choice.Listen
			} else if oldMode != config.TransportSSH && oldMode != config.TransportLocal {
				// Retain the effective listener, including a Tailscale-only
				// listener normalized from "none", across a socket-only round trip.
				document["listen"] = oldListen
			}
			if choice.Listen != config.DisabledListener {
				// Missing policy keys already mean default-capability access
				// for an established listener. Only seed a first activation.
				_, hasUsers := document["allow_users"]
				_, hasCapability := document["capability"]
				if !transport.tailnetConfigured && !hasUsers && !hasCapability {
					document["allow_users"] = choice.AllowUsers
				}
				if choice.TailscaledSocket != "" {
					document["tailscaled_socket"] = choice.TailscaledSocket
					delete(document, "tailscale_cli")
				}
				if choice.TailscaleCLI != "" {
					document["tailscale_cli"] = choice.TailscaleCLI
					delete(document, "tailscaled_socket")
				}
			}
			var encoded bytes.Buffer
			if err := toml.NewEncoder(&encoded).Encode(document); err != nil {
				return configPlan{}, err
			}
			rendered = encoded.String()
			effective = config.Daemon{MaxJobs: 1, MaxQueued: 8}
			if err := toml.Unmarshal([]byte(rendered), &effective); err != nil {
				return configPlan{}, err
			}
			configChanged = true
		}
	}

	return configPlan{rendered: rendered, changed: configChanged, effective: effective}, nil
}

func normalizeDaemonConfig(home string, d config.Daemon) (config.Daemon, error) {
	if err := d.NormalizeTransport(); err != nil {
		return d, err
	}
	if d.Listen == "" {
		d.Listen = fmt.Sprintf("tailnet:%d", DefaultPort)
	}
	if d.StateDir == "" {
		d.StateDir = filepath.Join(home, ".errand")
	}
	if d.MaxJobs <= 0 {
		return d, errors.New("max_jobs must be positive")
	}
	if d.MaxQueued < 0 {
		return d, errors.New("max_queued must not be negative")
	}
	return d, nil
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
