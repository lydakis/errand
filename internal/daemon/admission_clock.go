package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

const (
	jobIDMaxAge        = 24 * time.Hour
	jobIDMaxFutureSkew = time.Hour
	// A marker outlives the complete admissible ID window. Once it expires,
	// the ULID timestamp is necessarily too old relative to the durable clock.
	collectedMarkerTTL = jobIDMaxAge + jobIDMaxFutureSkew
)

type admissionClockRecord struct {
	HighWater time.Time `json:"high_water"`
}

func (d *Daemon) admissionClockPath() string {
	return filepath.Join(d.cfg.StateDir, "admission-clock.json")
}

func (d *Daemon) loadAdmissionClock() error {
	raw, err := os.ReadFile(d.admissionClockPath())
	if err == nil {
		var record admissionClockRecord
		if err := json.Unmarshal(raw, &record); err != nil || record.HighWater.IsZero() {
			return fmt.Errorf("invalid admission clock")
		}
		d.clockMu.Lock()
		d.admissionHighWater = record.HighWater
		d.clockMu.Unlock()
	} else if !os.IsNotExist(err) {
		return err
	}
	_, err = d.advanceAdmissionClock(time.Now())
	return err
}

func (d *Daemon) admissionNow(wall time.Time) time.Time {
	d.clockMu.Lock()
	defer d.clockMu.Unlock()
	if d.admissionHighWater.After(wall) {
		return d.admissionHighWater
	}
	return wall
}

func (d *Daemon) advanceAdmissionClock(wall time.Time) (time.Time, error) {
	d.clockMu.Lock()
	defer d.clockMu.Unlock()
	if !wall.After(d.admissionHighWater) {
		return d.admissionHighWater, nil
	}
	if err := replaceJSONDurable(d.admissionClockPath(), admissionClockRecord{HighWater: wall}); err != nil {
		return time.Time{}, err
	}
	d.admissionHighWater = wall
	return wall, nil
}

func validateNewJobID(jobID string, now time.Time) error {
	issuedAt, ok := proto.ULIDTimestamp(jobID)
	if !ok {
		return fmt.Errorf("job id must be a canonical ULID")
	}
	if !issuedAt.After(now.Add(-jobIDMaxAge)) {
		return fmt.Errorf("job id is too old for admission; check the client and runner clocks")
	}
	if issuedAt.After(now.Add(jobIDMaxFutureSkew)) {
		return fmt.Errorf("job id timestamp is too far in the future; check the client and runner clocks")
	}
	return nil
}
