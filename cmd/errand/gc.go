package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/termui"
)

func resolveGCPeerTarget(rawURL, on string) (string, string, error) {
	if rawURL != "" {
		return resolvePeerTarget(rawURL, on)
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return "", "", err
	}
	if on == "" {
		cfg = cfg.WithLocalPeer()
		switch len(cfg.Peers) {
		case 0:
			return "", "", errGCNoRunner
		case 1:
			for name := range cfg.Peers {
				on = name
			}
		default:
			return "", "", errGCPickRunner
		}
	}
	peerURL, err := configuredPeerURL(cfg, on)
	return peerURL, on, err
}

var (
	errGCNoRunner   = fmt.Errorf("no runners configured")
	errGCPickRunner = fmt.Errorf("pick a runner with --on; gc works on one at a time")
)

func cmdGC(args []string) int {
	return cmdGCTo(args, os.Stdout, os.Stderr)
}

var gcTargets = []string{"cache", "jobs", "changes", "all"}

// gcLine is one category's verdict.
type gcLine struct {
	where, what string
	freed       int64
	count       string // what would go or went, e.g. "604 jobs older than 7d"
	kept        string
	failed      string
}

func cmdGCTo(args []string, stdout, stderr io.Writer) int {
	con := newConsole(stdout, stderr)
	e, o := con.Err, con.Out
	if len(args) == 0 {
		printCommandHelp(stderr, "gc", flag.NewFlagSet("gc", flag.ContinueOnError))
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" {
		printCommandHelp(stdout, "gc", flag.NewFlagSet("gc", flag.ContinueOnError))
		return 0
	}
	target := args[0]
	if target != "cache" && target != "jobs" && target != "changes" && target != "all" {
		e.Errorf("unknown gc target '%s'", target)
		if guess := termui.Suggest(target, gcTargets); guess != "" {
			e.Hintf("did you mean errand gc %s?", guess)
		} else {
			e.Hintf("use cache, jobs, changes or all")
		}
		return 2
	}

	fs := flag.NewFlagSet("errand gc "+target, flag.ContinueOnError)
	var on, rawURL, olderThan string
	keep := -1
	dryRun := false
	fs.StringVar(&on, "on", "", "peer name")
	fs.StringVar(&rawURL, "url", "", "peer base URL")
	if target == "jobs" || target == "changes" || target == "all" {
		fs.StringVar(&olderThan, "older-than", "", "remove eligible data older than this duration")
	}
	if target == "jobs" || target == "all" {
		fs.IntVar(&keep, "keep", -1, "retain at least the newest N eligible jobs")
	}
	fs.BoolVar(&dryRun, "dry-run", false, "report eligible data without removing it")
	fs.BoolVar(&dryRun, "n", false, "report eligible data without removing it")
	var output outputFlags
	output.bind(fs, "")
	if ok, code := parseFlags(fs, args[1:], "gc "+target, stdout, e); !ok {
		return code
	}
	if fs.NArg() != 0 {
		return usageError(e, "unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	example := func() string {
		return "errand gc " + target + " --dry-run --on " + cmpOr(on, "cabal") + " --older-than 7d"
	}
	var jobRequest proto.JobGCRequest
	var retentionDuration time.Duration
	parseOlder := func() bool {
		duration, err := parseRetentionDuration(olderThan)
		if err != nil || duration < time.Second {
			e.Errorf("--older-than needs a duration of at least 1s, like 7d, 12h or 30m")
			return false
		}
		retentionDuration = duration
		return true
	}
	if target == "jobs" || target == "all" {
		if olderThan == "" && keep == -1 {
			if target == "all" {
				e.Errorf("gc all needs --older-than")
			} else {
				e.Errorf("gc jobs needs --older-than or --keep")
			}
			e.Hintf("for example %s", example())
			return 2
		}
		if olderThan != "" {
			if !parseOlder() {
				return 2
			}
			seconds := int64(retentionDuration / time.Second)
			if retentionDuration%time.Second != 0 {
				seconds++
			}
			jobRequest.OlderThanSeconds = &seconds
		}
		if keep != -1 {
			if keep < 0 {
				return usageError(e, "--keep can't be negative")
			}
			jobRequest.Keep = &keep
		}
		jobRequest.DryRun = dryRun
	}
	if target == "changes" {
		if olderThan == "" {
			e.Errorf("gc changes needs --older-than")
			e.Hintf("for example errand gc changes --dry-run --older-than 30d")
			return 2
		}
		if !parseOlder() {
			return 2
		}
	}
	if target == "all" && retentionDuration == 0 {
		e.Errorf("gc all needs --older-than, so local changes have a clear cutoff")
		e.Hintf("for example %s", example())
		return 2
	}
	peerURL, label := "", "local"
	if target != "changes" || on != "" || rawURL != "" {
		var err error
		peerURL, label, err = resolveGCPeerTarget(rawURL, on)
		if err != nil {
			switch err {
			case errGCPickRunner:
				e.Errorf("%v", err)
				e.Hintf("for example %s", example())
				return 2
			case errGCNoRunner:
				e.Errorf("no runners configured")
				e.Hintf("add one with errand peers add NAME HOST")
				return 1
			}
			return failWith(e, listErrorCode(err), err, errorScope{peer: on})
		}
	}
	quiet := output.quiet
	if dryRun && !quiet {
		o.Print(o.Paint("Dry run", termui.Yellow) + " " + o.D("· nothing is deleted"))
	}
	var spin *termui.Spinner
	if !quiet {
		where := label
		if target == "all" {
			where = label + " and this machine"
		}
		spin = e.Spin("Checking " + e.B(where) + "…")
	}
	stop := func() {
		if spin != nil {
			spin.Stop()
			spin = nil
		}
	}
	var lines []gcLine
	var policies []string
	failed := false
	fail := func(what string, err error) {
		stop()
		msg, _ := describeError(err, errorScope{peer: label})
		e.Errorf("%s: %s", what, msg)
		failed = true
	}
	age := ""
	if retentionDuration > 0 {
		age = " older than " + olderThan
	}
	if target == "cache" || target == "all" {
		result, err := client.CacheGC(peerURL, dryRun)
		if err != nil {
			fail("cache on "+label, err)
		} else {
			policies = append(policies, gcPolicies(label, result.Policies)...)
			var parts []string
			if result.RemovedBlobs > 0 {
				parts = append(parts, termui.Things(result.RemovedBlobs, "snapshot blob", "snapshot blobs"))
			}
			if result.RemovedCaches > 0 {
				parts = append(parts, termui.Things(result.RemovedCaches, "named cache", "named caches"))
			}
			policy := gcPolicySummary(result.Policies)
			count := joinWords(parts)
			kept := ""
			if count != "" {
				count += " past the runner's limits (" + policy + ")"
			} else {
				kept = policy
			}
			if result.ProtectedCaches > 0 {
				kept = termui.Count(result.ProtectedCaches) + " in use, kept"
			}
			lines = append(lines, gcLine{where: label, what: "caches", freed: result.FreedBytes, count: count, kept: kept})
		}
	}
	if target == "jobs" || target == "all" {
		result, err := client.JobGC(peerURL, jobRequest)
		if err != nil {
			fail("jobs on "+label, err)
		} else {
			removed := result.SelectedJobs - result.FailedJobs
			if !result.DryRun {
				removed = result.RemovedJobs
			}
			count := ""
			if removed > 0 {
				count = termui.Things(removed, "job", "jobs") + age
				if keep > 0 {
					count += " (keeping the newest " + termui.Count(keep) + ")"
				}
			}
			kept := ""
			if result.ProtectedJobs > 0 {
				kept = termui.Count(result.ProtectedJobs) + " kept (active or holding changes)"
			}
			line := gcLine{where: label, what: "jobs", freed: result.FreedBytes, count: count, kept: kept}
			if result.FailedJobs != 0 || result.CleanupFailures != 0 {
				var problems []string
				if result.FailedJobs != 0 {
					problems = append(problems, termui.Things(result.FailedJobs, "job couldn't be removed", "jobs couldn't be removed"))
				}
				if result.CleanupFailures != 0 {
					problems = append(problems, termui.Things(result.CleanupFailures, "cleanup failed", "cleanups failed"))
				}
				line.failed = strings.Join(problems, ", ")
				failed = true
			}
			lines = append(lines, line)
		}
		if !dryRun {
			if err := client.ReconcileCollectedJobChanges(peerURL); err != nil {
				fail("reconciling removed jobs' changes", err)
			}
		}
	}
	stale := 0
	if target == "all" || target == "changes" && peerURL == "" {
		result, err := client.ChangeGC(retentionDuration, dryRun)
		if err != nil {
			fail("fetched changes here", err)
		}
		stale = result.Stale
		if err == nil || result.Removed > 0 || result.Protected > 0 || result.Failed > 0 {
			count := ""
			if result.Removed > 0 {
				count = termui.Things(result.Removed, "record", "records") + age
			}
			kept := ""
			if result.Protected > 0 {
				kept = termui.Count(result.Protected) + " kept (pending or checkpointed)"
			}
			line := gcLine{where: "local", what: "fetched changes", freed: result.FreedBytes, count: count, kept: kept}
			if result.Failed != 0 {
				line.failed = termui.Things(result.Failed, "record failed", "records failed")
				failed = true
			}
			lines = append(lines, line)
		}
	}
	if target == "all" || target == "changes" && peerURL != "" {
		result, err := client.RemoteTransferGC(peerURL, retentionDuration, dryRun)
		if err != nil {
			fail("push staging on "+label, err)
		}
		if err == nil || len(result.Failures) > 0 {
			count := ""
			if result.Removed > 0 {
				count = termui.Things(result.Removed, "push", "pushes") + age
			}
			kept := ""
			if result.Protected > 0 {
				kept = termui.Count(result.Protected) + " kept (in progress)"
			}
			line := gcLine{where: label, what: "push staging", freed: result.FreedBytes, count: count, kept: kept}
			if len(result.Failures) > 0 {
				line.failed = termui.Things(len(result.Failures), "failure", "failures")
				failed = true
			}
			lines = append(lines, line)
		}
	}
	stop()
	if quiet {
		var total int64
		for _, l := range lines {
			total += l.freed
		}
		fmt.Fprintln(stdout, termui.Bytes(total))
	} else {
		writeGCLines(o, lines, dryRun)
		if stale > 0 {
			e.Warnf("skipped %s whose workspace was moved or deleted", termui.Things(stale, "local record", "local records"))
		}
		if output.verbose {
			for _, p := range policies {
				o.Print(o.D(p))
			}
		}
		if dryRun && !failed && gcTotal(lines) > 0 {
			o.Next("drop --dry-run to delete them", "")
		}
	}
	if failed {
		return 1
	}
	return 0
}

func gcTotal(lines []gcLine) int64 {
	var total int64
	for _, l := range lines {
		total += l.freed
	}
	return total
}

// writeGCLines says what gc freed or would free: one sentence for a single
// category, a table for several.
func writeGCLines(o *termui.Stream, lines []gcLine, dryRun bool) {
	verdict := func(l gcLine) (string, []termui.Attr) {
		switch {
		case l.failed != "" && dryRun:
			return "would free " + termui.Bytes(l.freed) + ", but " + l.failed, []termui.Attr{termui.Red}
		case l.failed != "":
			return "freed " + termui.Bytes(l.freed) + ", but " + l.failed, []termui.Attr{termui.Red}
		case l.count == "" && l.freed == 0:
			return "nothing to collect", []termui.Attr{termui.Dim}
		case dryRun:
			return "would free " + termui.Bytes(l.freed), []termui.Attr{termui.Bold}
		default:
			return "freed " + termui.Bytes(l.freed), []termui.Attr{termui.Bold, termui.Green}
		}
	}
	if len(lines) == 1 {
		l := lines[0]
		text, attrs := verdict(l)
		sentence := o.Paint(strings.ToUpper(text[:1])+text[1:], attrs...) + " on " + l.where
		if l.count != "" {
			sentence += ": " + l.count
		} else if l.failed == "" {
			sentence += " " + o.D("("+l.what+")")
		}
		if l.kept != "" {
			sentence += " " + o.D("· "+l.kept)
		}
		glyph := termui.OK
		if l.failed != "" {
			glyph = termui.Fail
		}
		if dryRun && l.failed == "" {
			o.Print(sentence)
		} else {
			o.Say(glyph, sentence)
		}
		return
	}
	t := o.Table()
	for _, l := range lines {
		text, attrs := verdict(l)
		detail := l.count
		if l.kept != "" {
			if detail != "" {
				detail += " · "
			}
			detail += l.kept
		}
		t.Row(termui.C(l.where, termui.Bold), termui.C(l.what), termui.C(text, attrs...), termui.C(detail, termui.Dim))
	}
	t.Print()
}

// gcPolicies describes the runner's expiry and budget, for -v.
func gcPolicies(label string, policies *proto.CacheGCPolicies) []string {
	if policies == nil {
		return nil
	}
	var out []string
	for _, category := range []struct {
		name   string
		policy *proto.CacheGCPolicy
	}{
		{"snapshot cache", policies.Snapshot}, {"named caches", policies.Named},
	} {
		if category.policy == nil {
			out = append(out, fmt.Sprintf("%s %s: collection disabled", label, category.name))
			continue
		}
		expiry := termui.Duration(time.Duration(category.policy.TTLSeconds) * time.Second)
		out = append(out, fmt.Sprintf("%s %s: expire after %s unused · budget %s", label, category.name, expiry, termui.Bytes(category.policy.MaxBytes)))
	}
	return out
}

// gcPolicySummary is the runner's cache limits in a few words.
func gcPolicySummary(p *proto.CacheGCPolicies) string {
	if p == nil {
		return ""
	}
	describe := func(policy *proto.CacheGCPolicy) string {
		if policy == nil {
			return "collection disabled"
		}
		return "expire after " + termui.Duration(time.Duration(policy.TTLSeconds)*time.Second) + " unused, budget " + termui.Bytes(policy.MaxBytes)
	}
	snapshot, named := describe(p.Snapshot), describe(p.Named)
	if snapshot == named {
		return snapshot
	}
	return "snapshot cache: " + snapshot + "; named caches: " + named
}
