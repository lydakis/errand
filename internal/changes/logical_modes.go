package changes

import (
	"github.com/lydakis/errand/internal/fsmode"
	"github.com/lydakis/errand/internal/proto"
)

// inheritBaselineModes keeps each path's submitted mode where the file
// system can't store POSIX modes (Windows). Otherwise every file would come
// back as 0644 and lose its exec bit. New paths keep the logical defaults.
func inheritBaselineModes(baseline proto.Manifest, current *proto.Manifest) {
	if !fsmode.Logical {
		return
	}
	recorded := make(map[string]proto.ManifestEntry, len(baseline.Entries))
	for _, entry := range baseline.Entries {
		recorded[entry.Path] = entry
	}
	for i := range current.Entries {
		entry := &current.Entries[i]
		prior, ok := recorded[entry.Path]
		switch {
		case !ok || prior.Type != entry.Type:
		case entry.Type == proto.EntryFile:
			entry.Mode = fsmode.Inherit(prior.Mode, entry.Mode)
		default:
			entry.Mode = prior.Mode
		}
	}
}
