package logio

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/lydakis/errand/internal/proto"
)

// Window is where a bounded replay of io.log begins.
type Window struct {
	// Start is the first frame to replay; Last+1 when nothing qualifies.
	Start int64
	// Cut is how many leading bytes of frame Start to drop, so a tail starts
	// at the beginning of a line.
	Cut int
	// Last is the highest complete frame in the file when it was read.
	Last int64
}

// FindWindow reads the complete frames in path and returns where a replay
// begins: at the first frame written at or after sinceMS (0 means from the
// start), narrowed to the last tail lines from there on (tail < 0 means no
// limit). Lines are counted across stdout and stderr together, in the order
// the runner saw them; output without a final newline counts as a line.
//
// live is the log's writer, or nil when it belongs to an earlier run of the
// daemon, as in Follow. While live is open, a record still being written at
// the end of the file is left out; once the log is finished, one is an
// integrity error.
//
// It reads the file twice, once to count lines and once to find where the
// tail starts, so memory stays constant however long the lines are.
func FindWindow(path string, sinceMS int64, tail int, live *Writer) (Window, error) {
	complete := live == nil || live.isClosed()
	window := Window{Start: -1}
	var lines int64 // newlines from Start on
	endsWithNL := false
	last, err := scanFrames(path, -1, complete, func(frame proto.LogFrame) error {
		if window.Start < 0 {
			if frame.TUnixMS < sinceMS {
				return nil
			}
			window.Start = frame.Seq
		}
		if tail <= 0 {
			return nil
		}
		data, err := base64.StdEncoding.DecodeString(frame.DataB64)
		if err != nil {
			return integrityError(err)
		}
		if len(data) > 0 {
			lines += int64(bytes.Count(data, []byte{'\n'}))
			endsWithNL = data[len(data)-1] == '\n'
		}
		return nil
	})
	if err != nil {
		return Window{}, err
	}
	window.Last = last
	if window.Start < 0 || tail == 0 {
		return Window{Start: last + 1, Last: last}, nil
	}
	if tail < 0 {
		return window, nil
	}
	// A final newline ends the last line rather than starting one, so the
	// tail starts after newline number lines-tail (or lines-tail+1 without a
	// final newline), counting from Start.
	extra := int64(0)
	if endsWithNL {
		extra = 1
	}
	if lines-extra < int64(tail) {
		return window, nil // fewer lines than asked for
	}
	target := lines - extra - int64(tail) + 1
	var seen int64
	found := Window{Start: last + 1, Last: last}
	_, err = scanFrames(path, last, complete, func(frame proto.LogFrame) error {
		if frame.Seq < window.Start {
			return nil
		}
		data, err := base64.StdEncoding.DecodeString(frame.DataB64)
		if err != nil {
			return integrityError(err)
		}
		for i := 0; i < len(data); i++ {
			if data[i] != '\n' {
				continue
			}
			if seen++; seen < target {
				continue
			}
			if i+1 < len(data) {
				found.Start, found.Cut = frame.Seq, i+1
			} else {
				found.Start = frame.Seq + 1
			}
			return errFound
		}
		return nil
	})
	if err != nil && !errors.Is(err, errFound) {
		return Window{}, err
	}
	return found, nil
}

var errFound = errors.New("found")

// scanFrames calls fn with each complete frame in path, in order, checking
// that sequence numbers run from 1 without gaps, and stops after frame stop
// when stop >= 0. It returns the last frame it read. A partial record at the
// end of the file is skipped unless complete is set, when it is an integrity
// error.
func scanFrames(path string, stop int64, complete bool, fn func(proto.LogFrame) error) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var last int64
	r := bufio.NewReaderSize(f, 64<<10)
	for stop < 0 || last < stop {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			if complete && len(line) > 0 {
				return last, integrityError(io.ErrUnexpectedEOF)
			}
			break
		}
		if err != nil {
			return last, err
		}
		var frame proto.LogFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			return last, integrityError(err)
		}
		if frame.Seq != last+1 {
			return last, integrityError(fmt.Errorf("log sequence %d, expected %d", frame.Seq, last+1))
		}
		last = frame.Seq
		if err := fn(frame); err != nil {
			return last, err
		}
	}
	return last, nil
}

// TrimFrame drops the first cut bytes of a frame's data.
func TrimFrame(frame proto.LogFrame, cut int) (proto.LogFrame, error) {
	if cut == 0 {
		return frame, nil
	}
	data, err := base64.StdEncoding.DecodeString(frame.DataB64)
	if err != nil {
		return frame, integrityError(err)
	}
	if cut > len(data) {
		return frame, integrityError(fmt.Errorf("log frame %d is shorter than its replay cut", frame.Seq))
	}
	frame.DataB64 = base64.StdEncoding.EncodeToString(data[cut:])
	return frame, nil
}
