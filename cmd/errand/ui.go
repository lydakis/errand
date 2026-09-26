package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/lydakis/errand/internal/client"
	"github.com/lydakis/errand/internal/config"
	"github.com/lydakis/errand/internal/termui"
)

// newConsole detects terminal features for one invocation's streams.
func newConsole(stdout, stderr io.Writer) *termui.Console {
	return termui.Detect(stdout, stderr)
}

// outputFlags are the verbosity flags every command accepts.
type outputFlags struct {
	quiet, verbose bool
}

func (o *outputFlags) bind(fs *flag.FlagSet, verboseUsage string) {
	fs.BoolVar(&o.quiet, "quiet", false, "print only data and errors")
	fs.BoolVar(&o.quiet, "q", false, "print only data and errors")
	if verboseUsage == "" {
		verboseUsage = "show every detail"
	}
	fs.BoolVar(&o.verbose, "verbose", false, verboseUsage)
	fs.BoolVar(&o.verbose, "v", false, verboseUsage)
}

// displayHandle is how a job is named on screen: short on a terminal, full
// wherever it may be copied into a script or log.
func displayHandle(s *termui.Stream, peer, id string) string {
	if s.Interactive() {
		return peer + "/" + termui.ShortID(id)
	}
	return peer + "/" + id
}

// errorScope names what a command was operating on, so failures can be
// phrased in the reader's terms.
type errorScope struct {
	peer      string
	job       string
	workspace string
}

// failWith prints an error: line (and a hint: line when one helps) and
// returns code.
func failWith(s *termui.Stream, code int, err error, scope errorScope) int {
	msg, hint := describeError(err, scope)
	s.Errorf("%s", msg)
	if hint != "" {
		s.Hintf("%s", hint)
	}
	return code
}

// usageError prints a usage mistake and returns exit code 2.
func usageError(s *termui.Stream, format string, args ...any) int {
	s.Errorf(format, args...)
	return 2
}

// describeError turns transport and runner errors into a sentence about the
// thing the reader asked for, plus the next step when there is one.
func describeError(err error, scope errorScope) (string, string) {
	var badHandle *badHandleError
	if errors.As(err, &badHandle) {
		return badHandle.Error(), "handles look like mini/01M3BFTQ6QD4; errand ps lists them"
	}
	var unknown *config.UnknownPeerError
	if errors.As(err, &unknown) {
		return "no runner named " + unknown.Name, knownRunnersHint(unknown.Name)
	}
	var prefix *client.JobPrefixError
	if errors.As(err, &prefix) {
		where := cmpOr(scope.peer, "the runner")
		if len(prefix.Matches) == 0 {
			return fmt.Sprintf("%s has no job starting with %s", where, prefix.Prefix),
				"errand ps -a --on " + cmpOr(scope.peer, "PEER") + " lists what's there"
		}
		return fmt.Sprintf("%s matches %d jobs on %s", prefix.Prefix, len(prefix.Matches), where), "use more characters of the job id"
	}
	if status, message, ok := client.RemoteMessage(err); ok {
		switch {
		case status == http.StatusNotFound && scope.job != "":
			return fmt.Sprintf("%s has no job %s", cmpOr(scope.peer, "the runner"), scope.job),
				"it may have been removed by gc · errand ps -a --on " + cmpOr(scope.peer, "PEER") + " lists what's there"
		case status == http.StatusNotFound && scope.workspace != "":
			return fmt.Sprintf("%s has no workspace named %s", cmpOr(scope.peer, "the runner"), scope.workspace),
				"errand workspaces lists them on every runner"
		case status == http.StatusForbidden:
			return fmt.Sprintf("%s refused this request: %s", cmpOr(scope.peer, "the runner"), message),
				"check the runner's access list with errand access list on that machine"
		case message != "":
			return message, ""
		}
	}
	if isUnreachable(err) {
		return fmt.Sprintf("couldn't reach %s (%s)", cmpOr(scope.peer, "the runner"), unreachableCause(err)),
			"check that it's online with errand peers"
	}
	return err.Error(), ""
}

func knownRunnersHint(missing string) string {
	cfg, err := config.LoadClient()
	if err != nil || len(cfg.Peers) == 0 {
		return "add one with errand peers add " + missing + " HOST"
	}
	names := make([]string, 0, len(cfg.Peers))
	for name := range cfg.Peers {
		names = append(names, name)
	}
	sort.Strings(names)
	if guess := termui.Suggest(missing, names); guess != "" {
		return "did you mean " + guess + "? You have " + joinWords(names)
	}
	return "you have " + joinWords(names) + " · errand peers add " + missing + " HOST to add one"
}

// joinWords renders "a", "a and b", "a, b and c".
func joinWords(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	default:
		return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
	}
}

func isUnreachable(err error) bool {
	var urlErr *url.Error
	var netErr *net.OpError
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr) || errors.As(err, &netErr) ||
		errors.As(err, &urlErr) && (urlErr.Timeout() || errors.Is(err, syscall.ECONNREFUSED))
}

func unreachableCause(err error) string {
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		return "no such host"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "no route to host"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return "timed out"
	}
	return "network error"
}

func truncateString(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func padRight(s string, width int) string {
	if n := termui.CellWidth(s); n < width {
		return s + strings.Repeat(" ", width-n)
	}
	return s
}

func msDuration(ms int64) time.Duration { return time.Duration(ms) * time.Millisecond }
