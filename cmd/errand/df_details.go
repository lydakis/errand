package main

import (
	"fmt"
	"sort"
	"time"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/termui"
)

// writeDfDetails breaks each location down, largest first, folding the
// small items into one line and leaving empty categories out.
func writeDfDetails(s *termui.Stream, rows []dfRow) {
	const listed = 5
	label := func(text string) string { return "  " + s.D(padRight(text, 15)) + " " }
	indent := "  " + padRight("", 15) + " "
	for _, row := range rows {
		s.Print("")
		s.Print(s.B(terminalSafeField(row.Location)) + " " + s.D("· "+termui.Bytes(row.TotalBytes)))
		d := row.Details
		if row.hasRunner && row.Jobs.Items > 0 {
			s.Print(label("jobs") + termui.Bytes(row.Jobs.Bytes) + " in " + termui.Count(row.Jobs.Items))
			if d != nil {
				jobs := append([]proto.JobStorage(nil), d.Jobs...)
				sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].Bytes > jobs[j].Bytes })
				var rest int64
				for i, job := range jobs {
					if i >= listed {
						rest += job.Bytes
						continue
					}
					extra := ""
					if job.CleanupPending {
						extra = " " + s.Paint("cleanup pending", termui.Yellow)
					}
					s.Print(indent + s.ID(termui.ShortID(job.ID)) + "  " + termui.Bytes(job.Bytes) + extra)
				}
				if more := len(jobs) - listed; more > 0 {
					s.Print(indent + s.D(fmt.Sprintf("%d more, %s together", more, termui.Bytes(rest))))
				}
			}
		}
		if row.Cache != nil {
			policy := ""
			if row.Cache.MaxBytes > 0 {
				policy = " · budget " + termui.Bytes(row.Cache.MaxBytes)
			}
			if row.Cache.TTLHours > 0 {
				policy += " · unused blobs expire after " + termui.Duration(time.Duration(row.Cache.TTLHours)*time.Hour)
			}
			s.Print(label("snapshot cache") + termui.Bytes(row.Cache.Bytes) + " in " + termui.Things(row.Cache.Blobs, "blob", "blobs") + s.D(policy))
		}
		if row.NamedCaches != nil && row.NamedCaches.Items > 0 {
			line := termui.Bytes(row.NamedCaches.Bytes) + " in " + termui.Count(row.NamedCaches.Items)
			if row.NamedCaches.Unmeasured > 0 {
				line += s.D(fmt.Sprintf(" · %d not measured yet", row.NamedCaches.Unmeasured))
			}
			if row.NamedCaches.Protected > 0 {
				line += s.D(fmt.Sprintf(" · %d in use", row.NamedCaches.Protected))
			}
			s.Print(label("named caches") + line)
			if d != nil {
				caches := append([]proto.NamedCacheStorage(nil), d.NamedCaches...)
				sort.SliceStable(caches, func(i, j int) bool { return caches[i].Bytes > caches[j].Bytes })
				for i, c := range caches {
					if i >= listed {
						s.Print(indent + s.D(fmt.Sprintf("%d more", len(caches)-listed)))
						break
					}
					size := termui.Bytes(c.Bytes)
					if c.BytesUnknown {
						size = s.D("not measured")
					}
					s.Print(indent + terminalSafeField(c.Name) + "  " + size)
				}
			}
		}
		if row.Workspaces != nil && row.Workspaces.Items > 0 {
			s.Print(label("workspaces") + termui.Bytes(row.Workspaces.Bytes) + " in " + termui.Count(row.Workspaces.Items))
			if d != nil {
				for _, w := range d.Workspaces {
					parts := fmt.Sprintf("files %s · base %s · transfers %s · metadata %s", termui.Bytes(w.WorkingBytes), termui.Bytes(w.BaseBytes), termui.Bytes(w.TransferBytes), termui.Bytes(w.MetadataBytes))
					s.Print(indent + s.B(terminalSafeField(w.Name)) + "  " + termui.Bytes(w.Bytes) + " " + s.D("· "+parts))
				}
			}
		}
		if row.Changes != nil && row.Changes.Items > 0 {
			what := "push staging"
			if row.Location == "local" {
				what = "fetched changes"
			}
			s.Print(label(what) + termui.Bytes(row.Changes.Bytes) + " in " + termui.Things(row.Changes.Items, "entry", "entries"))
		}
	}
	s.Print("")
	s.Print(s.D("Logical sizes; cloned files may share disk blocks."))
}
