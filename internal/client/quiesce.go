package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/lydakis/errand/internal/proto"
)

// ErrRunnerNotIdle is QuiesceRunner's answer when the runner has jobs, or
// is already held idle.
var ErrRunnerNotIdle = errors.New("runner is not idle")

// ErrNotHeld is ResumeRunner's answer when the runner has no hold with that
// token: it lapsed, was lifted, or was never taken.
var ErrNotHeld = errors.New("runner has no such hold")

// ErrQuiesceRefused is QuiesceRunner's answer when the runner does not let
// the caller hold it, such as a caller that is not its own user.
var ErrQuiesceRefused = errors.New("runner refused to be held idle")

// QuiesceRunner has an idle runner refuse new jobs for a few minutes, as
// errand setup does before a restart, and returns the token that ends it
// early. A nonempty token, a ULID, names the hold, so the caller can record
// it first: a live hold with that token is renewed, and otherwise the hold
// is taken under it. A runner admits this only from its own user over its
// local socket, so only a peer reached over SSH can be held this way.
func QuiesceRunner(ctx context.Context, peerURL, token string) (string, error) {
	var payload io.Reader = http.NoBody
	if token != "" {
		raw, _ := json.Marshal(proto.SetupQuiesceRequest{Token: token})
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(peerURL, "/")+"/v0/setup/quiesce", payload)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := directHTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	body, err := readBoundedBody(res.Body, 4096, "quiesce")
	if err != nil {
		return "", err
	}
	switch res.StatusCode {
	case http.StatusCreated:
	case http.StatusConflict:
		return "", fmt.Errorf("%w: %s", ErrRunnerNotIdle, apiError(body))
	case http.StatusForbidden, http.StatusNotFound:
		return "", fmt.Errorf("%w: %s", ErrQuiesceRefused, apiError(body))
	default:
		return "", &controlHTTPError{statusCode: res.StatusCode, err: errors.New(apiError(body))}
	}
	var hold proto.SetupQuiesce
	if err := json.Unmarshal(body, &hold); err != nil || hold.Token == "" {
		return "", errors.New("runner returned no quiesce token")
	}
	return hold.Token, nil
}

// ResumeRunner ends a hold QuiesceRunner made.
func ResumeRunner(ctx context.Context, peerURL, token string) error {
	body, _ := json.Marshal(proto.SetupQuiesceRelease{Token: token})
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimSuffix(peerURL, "/")+"/v0/setup/quiesce", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := directHTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := readBoundedBody(res.Body, 4096, "quiesce")
	if err != nil {
		return err
	}
	switch res.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrNotHeld, apiError(raw))
	}
	return &controlHTTPError{statusCode: res.StatusCode, err: errors.New(apiError(raw))}
}
