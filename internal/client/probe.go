package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

// ProbeKind classifies why a peer probe did not yield a runner.
type ProbeKind string

const (
	ProbeRunner      ProbeKind = "runner"      // answered /v0/info
	ProbeForbidden   ProbeKind = "forbidden"   // an errand runner refused this caller
	ProbeUnreachable ProbeKind = "unreachable" // nothing answered
	ProbeNotErrand   ProbeKind = "not-errand"  // something answered, not errand
)

// ProbeError classifies a failed peer probe.
type ProbeError struct {
	Kind   ProbeKind
	Status int
	Detail string
}

func (e *ProbeError) Error() string {
	switch e.Kind {
	case ProbeForbidden:
		return "runner refused this caller: " + e.Detail
	case ProbeNotErrand:
		return "not an errand runner: " + e.Detail
	default:
		return "unreachable: " + e.Detail
	}
}

// ProbeInfo fetches and validates a peer's /v0/info response.
func ProbeInfo(ctx context.Context, peerURL string, timeout time.Duration) (proto.Info, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(peerURL, "/")+"/v0/info", nil)
	if err != nil {
		return proto.Info{}, &ProbeError{Kind: ProbeUnreachable, Detail: err.Error()}
	}
	res, err := directHTTP.Do(req)
	if err != nil {
		return proto.Info{}, &ProbeError{Kind: ProbeUnreachable, Detail: shortNetErr(err)}
	}
	defer res.Body.Close()
	body, err := readBoundedBody(res.Body, 1<<20, "runner info")
	if err != nil {
		return proto.Info{}, &ProbeError{Kind: ProbeNotErrand, Status: res.StatusCode, Detail: err.Error()}
	}
	switch res.StatusCode {
	case http.StatusOK:
		var info proto.Info
		if err := json.Unmarshal(body, &info); err != nil || info.Version == "" {
			return proto.Info{}, &ProbeError{Kind: ProbeNotErrand, Status: res.StatusCode, Detail: "answered 200 without an errand info document"}
		}
		var protocol struct {
			Proto *int `json:"proto"`
		}
		if err := json.Unmarshal(body, &protocol); err != nil || protocol.Proto == nil || *protocol.Proto != proto.ProtoVersion {
			return proto.Info{}, &ProbeError{Kind: ProbeNotErrand, Status: res.StatusCode, Detail: "answered 200 without the current errand protocol"}
		}
		return info, nil
	case http.StatusForbidden:
		return proto.Info{}, &ProbeError{Kind: ProbeForbidden, Status: res.StatusCode, Detail: apiError(body)}
	default:
		detail := apiError(body)
		if detail == "" {
			detail = res.Status
		}
		return proto.Info{}, &ProbeError{Kind: ProbeNotErrand, Status: res.StatusCode, Detail: detail}
	}
}

func shortNetErr(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 && strings.HasPrefix(msg, "Get ") {
		return msg[i+2:]
	}
	return msg
}

// ProbeKindOf extracts the classification from an error, if any.
func ProbeKindOf(err error) (ProbeKind, bool) {
	var pe *ProbeError
	if errors.As(err, &pe) {
		return pe.Kind, true
	}
	return "", false
}
