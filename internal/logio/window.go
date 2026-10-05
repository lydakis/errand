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

// FindWindow reads the complete frames in path and returns where a replay of
// the frames written at or after sinceMS (0 means from the start) begins,
// narrowed to their last tail lines (tail < 0 means no limit). Lines are
// counted across stdout and stderr together, in the order the runner saw
// them; output without a final newline counts as a line. A record still being
// written at the end of the file is left out.
func FindWindow(path string, sinceMS int64, tail int) (Window, error) {
	f, err := os.Open(path)
	if err != nil {
		return Window{}, err
	}
	defer f.Close()

	type kept struct {
		seq  int64
		data []byte
		nl   int
	}
	var frames []kept
	keptNL := 0 // newlines in frames[1:]
	window := Window{Start: -1}
	r := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break // a partial record is still being written
		}
		if err != nil {
			return Window{}, err
		}
		var frame proto.LogFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			return Window{}, integrityError(err)
		}
		if frame.Seq != window.Last+1 {
			return Window{}, integrityError(fmt.Errorf("log sequence %d, expected %d", frame.Seq, window.Last+1))
		}
		window.Last = frame.Seq
		if frame.TUnixMS < sinceMS {
			continue
		}
		if window.Start < 0 {
			window.Start = frame.Seq
		}
		if tail < 0 {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(frame.DataB64)
		if err != nil {
			return Window{}, integrityError(err)
		}
		nl := bytes.Count(data, []byte{'\n'})
		if len(frames) > 0 {
			keptNL += nl
		}
		frames = append(frames, kept{seq: frame.Seq, data: data, nl: nl})
		// frames[1:] alone holds enough lines, so frames[0] can never be
		// part of the tail.
		for len(frames) > 1 && keptNL >= tail+1 {
			frames = frames[1:]
			keptNL -= frames[0].nl
		}
	}
	if window.Start < 0 || tail == 0 {
		return Window{Start: window.Last + 1, Last: window.Last}, nil
	}
	if tail < 0 {
		return window, nil
	}

	// Find the start of the tail-th line from the end, walking back over the
	// kept frames; a final newline ends the last line rather than starting one.
	need := tail
	last := frames[len(frames)-1].data
	for i := len(frames) - 1; len(last) == 0 && i > 0; i-- {
		last = frames[i-1].data
	}
	if len(last) > 0 && last[len(last)-1] == '\n' {
		need++
	}
	seen := 0
	for i := len(frames) - 1; i >= 0; i-- {
		data := frames[i].data
		for j := len(data) - 1; j >= 0; j-- {
			if data[j] != '\n' {
				continue
			}
			seen++
			if seen < need {
				continue
			}
			if j+1 < len(data) {
				return Window{Start: frames[i].seq, Cut: j + 1, Last: window.Last}, nil
			}
			if i+1 < len(frames) {
				return Window{Start: frames[i+1].seq, Last: window.Last}, nil
			}
			return Window{Start: window.Last + 1, Last: window.Last}, nil
		}
	}
	return Window{Start: frames[0].seq, Last: window.Last}, nil
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
