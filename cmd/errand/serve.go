package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/daemon"
	"github.com/lydakis/errand/internal/serviceruntime"
	"github.com/lydakis/errand/internal/tailnet"
	"github.com/lydakis/errand/internal/termui"
)

// serveLog writes the runner's log: a readable block and one line per job on
// a terminal, key=value lines under launchd or systemd.
type serveLog struct {
	e *termui.Stream
}

func (l serveLog) kv(level, msg string, fields ...string) {
	var b strings.Builder
	fmt.Fprintf(&b, "time=%s level=%s msg=%q", time.Now().Format(time.RFC3339), level, msg)
	for i := 0; i+1 < len(fields); i += 2 {
		value := fields[i+1]
		if value == "" || strings.ContainsAny(value, " \"=") || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			value = fmt.Sprintf("%q", value)
		}
		fmt.Fprintf(&b, " %s=%s", fields[i], value)
	}
	l.e.Print(b.String())
}

// fatal reports a startup failure and exits.
func (l serveLog) fatal(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if l.e.Interactive() {
		l.e.Errorf("%s", msg)
	} else {
		l.kv("error", msg)
	}
	os.Exit(1)
}

func (l serveLog) ready(listen, auth, socket, socketUse, state string, slots, queue int) {
	if !l.e.Interactive() {
		fields := []string{"version", version}
		if listen != "" {
			fields = append(fields, "listen", listen, "auth", auth)
		}
		fields = append(fields, "socket", socket, "socket_use", socketUse, "state", state, "slots", fmt.Sprint(slots), "queue", fmt.Sprint(queue))
		l.kv("info", "runner ready", fields...)
		return
	}
	e := l.e
	e.Print(e.B("errand "+version) + " runner " + e.Paint("ready", termui.Green))
	field := func(label, value string) { e.Print("  " + e.D(padRight(label, 7)) + " " + value) }
	if listen != "" {
		field("Tailnet", "http://"+listen+" "+e.D("· "+auth))
	}
	field("Socket", homeRelative(socket)+" "+e.D("· "+socketUse))
	field("State", homeRelative(state)+" "+e.D("· "+termui.Things(slots, "slot", "slots")+" · queue of "+termui.Count(queue)))
	e.Print("")
}

func (l serveLog) job(event daemon.JobLogEvent) {
	who := event.Owner
	if i := strings.IndexByte(who, '@'); i > 0 {
		who = who[:i]
	}
	command := termui.ShellQuote(event.Argv)
	if !l.e.Interactive() {
		fields := []string{"job", event.ID}
		switch event.Kind {
		case daemon.JobLogQueued:
			fields = append(fields, "user", event.Owner, "ahead", fmt.Sprint(event.QueueAhead))
		case daemon.JobLogStarted:
			fields = append(fields, "user", event.Owner, "project", event.Project, "argv", command)
		case daemon.JobLogFinished:
			fields = append(fields, serveOutcomeFields(event)...)
		}
		l.kv("info", "job "+string(event.Kind), fields...)
		return
	}
	e := l.e
	clock := e.D(event.Time.Format("15:04:05"))
	id := e.ID(termui.ShortID(event.ID))
	switch event.Kind {
	case daemon.JobLogQueued:
		e.Print(clock + " " + e.G(termui.Wait) + " " + id + " " + terminalSafeField(who) + " " + e.D("· queued, "+termui.Things(event.QueueAhead, "job", "jobs")+" ahead"))
	case daemon.JobLogStarted:
		project := ""
		if event.Project != "" {
			project = " " + e.D("· "+terminalSafeField(event.Project))
		}
		e.Print(clock + " " + e.G(termui.Here) + " " + id + " " + terminalSafeField(who) + " " + e.D("·") + " " + terminalSafeField(termui.Truncate(command, 80)) + project)
	case daemon.JobLogFinished:
		glyph, text := serveOutcome(event)
		e.Print(clock + " " + e.G(glyph) + " " + id + " " + text)
	}
}

func serveOutcome(event daemon.JobLogEvent) (termui.Glyph, string) {
	res := event.Result
	if res == nil {
		return termui.Warn, "finished without a result"
	}
	ran := termui.Duration(time.Duration(res.DurationMS) * time.Millisecond)
	switch {
	case res.StartError != "":
		return termui.Fail, "couldn't start: " + terminalSafeField(res.StartError)
	case res.Signal != "" && !res.Started:
		return termui.Fail, "killed by " + client.SignalName(res.Signal, res.SignalNum) + " before the command started"
	case res.Signal != "":
		return termui.Fail, "killed by " + client.SignalName(res.Signal, res.SignalNum) + " after " + ran
	case res.ExitCode != nil && *res.ExitCode == 0:
		return termui.OK, "exited 0 in " + ran
	case res.ExitCode != nil:
		return termui.Fail, fmt.Sprintf("exited %d in %s", *res.ExitCode, ran)
	default:
		return termui.Warn, "finished without a process outcome"
	}
}

func serveOutcomeFields(event daemon.JobLogEvent) []string {
	res := event.Result
	if res == nil {
		return []string{"outcome", "unknown"}
	}
	fields := []string{"duration", termui.Duration(time.Duration(res.DurationMS) * time.Millisecond)}
	switch {
	case res.StartError != "":
		fields = append(fields, "start_error", res.StartError)
	case res.Signal != "":
		fields = append(fields, "signal", client.SignalName(res.Signal, res.SignalNum))
		if !res.Started {
			fields = append(fields, "started", "false")
		}
	case res.ExitCode != nil:
		fields = append(fields, "exit", fmt.Sprint(*res.ExitCode))
	}
	if res.TransactionError != "" {
		fields = append(fields, "transaction_error", res.TransactionError)
	}
	return fields
}

func cmdServe(args []string) int { return cmdServeTo(args, os.Stdout, os.Stderr) }

func cmdServeTo(args []string, stdout, stderr io.Writer) int {
	con := newConsole(stdout, stderr)
	e := con.Err
	logger := serveLog{e: e}
	fs := flag.NewFlagSet("errand serve", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to errandd.toml")
	listen := fs.String("listen", "", `listen address ("tailnet:7443" resolves the tailnet IP; "none" disables TCP)`)
	stateDir := fs.String("state-dir", "", "receipt and job state directory")
	insecure := fs.Bool("insecure-no-auth", false, "DANGEROUS: skip all authorization (tests only)")
	var allowUsers stringList
	fs.Var(&allowUsers, "allow-user", "tailnet login allowed to use this runner (repeatable)")
	if ok, code := parseFlags(fs, args, "serve", stdout, e); !ok {
		return code
	}
	// The daemon's own diagnostics go through the standard logger; keep them
	// in the same shape as ours.
	log.SetFlags(0)
	log.SetOutput(serveStdLog{logger})

	fileCfg, err := config.LoadDaemon(*cfgPath)
	if err != nil {
		logger.fatal("%v", err)
	}
	if fileCfg.Transport == config.TransportLocal && *listen != "" && !strings.EqualFold(strings.TrimSpace(*listen), config.DisabledListener) {
		e.Errorf("this runner is set to local jobs only, so it can't listen on the network")
		e.Hintf("change transport in %s and rerun errand setup", homeRelative(cmpOr(*cfgPath, "~/.config/errand/errandd.toml")))
		return 2
	}
	if *listen != "" {
		fileCfg.Listen = *listen
	}
	if *stateDir != "" {
		fileCfg.StateDir = *stateDir
	}
	fileCfg.AllowUsers = append(fileCfg.AllowUsers, allowUsers...)

	// The service manager still launches the installed CLI. Before listening,
	// move execution to immutable bytes that package cleanup cannot unlink.
	if err := serviceruntime.Reexec(fileCfg.StateDir); err != nil {
		logger.fatal("prepare runtime: %v", err)
	}

	tcpEnabled := !strings.EqualFold(strings.TrimSpace(fileCfg.Listen), config.DisabledListener)
	var identity tailnet.Provider
	var addr string
	if tcpEnabled {
		addr, identity, err = resolveServeTransport(
			fileCfg.Listen, *insecure, fileCfg.TailscaledSocket, fileCfg.TailscaleCLI, tailnet.Discover,
		)
		if err != nil {
			logger.fatal("%v", err)
		}
	}
	d, err := daemon.New(daemon.Config{
		ChangeStorage:      client.ChangeStorageStats,
		DisableSSH:         fileCfg.Transport == config.TransportTailscale,
		LocalOnly:          fileCfg.Transport == config.TransportLocal,
		Listen:             addr,
		StateDir:           fileCfg.StateDir,
		AllowUsers:         fileCfg.AllowUsers,
		DenyUsers:          fileCfg.DenyUsers,
		Capability:         fileCfg.Capability,
		TailscaledSocket:   fileCfg.TailscaledSocket,
		Identity:           identity,
		InsecureNoAuth:     *insecure,
		Version:            version,
		CacheDisabled:      fileCfg.Cache.Disabled,
		CacheMaxBytes:      fileCfg.Cache.MaxBytes,
		NamedCacheDisabled: fileCfg.NamedCache.Disabled, NamedCacheMaxBytes: fileCfg.NamedCache.MaxBytes, NamedCacheTTL: time.Duration(fileCfg.NamedCache.TTLHours) * time.Hour,
		CacheTTL:  time.Duration(fileCfg.Cache.TTLHours) * time.Hour,
		MaxJobs:   fileCfg.MaxJobs,
		MaxQueued: fileCfg.MaxQueued,
		JobLog:    logger.job,
	})
	if err != nil {
		logger.fatal("%v", err)
	}
	defer d.Close()
	handler := d.Handler()
	socketPath := fileCfg.SocketPath()
	unixListener, err := listenUnixSocket(socketPath)
	if err != nil {
		logger.fatal("%v", err)
	}
	defer os.Remove(socketPath)
	auth := "no authorization (insecure)"
	if identity != nil {
		auth = "callers identified by " + identity.Name()
	}
	socketUse := "SSH and local jobs"
	switch {
	case fileCfg.Transport == config.TransportLocal:
		socketUse = "local jobs only"
	case tcpEnabled && fileCfg.Transport == config.TransportTailscale:
		socketUse = "local control only"
	}
	slots, queue := max(fileCfg.MaxJobs, 1), max(fileCfg.MaxQueued, 0)
	if !tcpEnabled {
		addr = ""
	}
	logger.ready(addr, auth, socketPath, socketUse, fileCfg.StateDir, slots, queue)

	unixServer := &http.Server{
		Handler:           handler,
		ConnContext:       daemon.ConnContext,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	errs := make(chan error, 2)
	go func() { errs <- unixServer.Serve(unixListener) }()
	if tcpEnabled {
		tcpServer := &http.Server{
			Addr: addr, Handler: handler,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Minute,
			IdleTimeout:       2 * time.Minute,
		}
		go func() { errs <- tcpServer.ListenAndServe() }()
	}
	if err := <-errs; err != nil {
		logger.fatal("%v", err)
	}
	return 0
}

// serveStdLog adapts the standard logger (used inside the daemon) to the
// runner log's format.
type serveStdLog struct{ l serveLog }

func (w serveStdLog) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if w.l.e.Interactive() {
		w.l.e.Print(w.l.e.D(time.Now().Format("15:04:05")) + " " + msg)
	} else {
		w.l.kv("info", msg)
	}
	return len(p), nil
}
