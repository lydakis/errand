package main

import (
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
)

// Unix targets share this machine with the invoking client. Remote peers keep
// their configured labels; storage identities are never compared across hosts.
func storageRows(results []peerQueryResult[proto.StorageStats], changes *proto.ChangeStorageStats) []dfRow {
	rows := make([]dfRow, 0, len(results)+1)
	localIndex := -1
	localRow := func() *dfRow {
		if localIndex < 0 {
			localIndex = len(rows)
			rows = append(rows, dfRow{Location: "local"})
		}
		return &rows[localIndex]
	}
	seenTargets := map[string]bool{}
	seenChanges := map[string]bool{}
	addChanges := func(row *dfRow, stats *proto.ChangeStorageStats, local bool) {
		if stats == nil {
			return
		}
		if local && stats.StoreID != "" {
			if seenChanges[stats.StoreID] {
				return
			}
			seenChanges[stats.StoreID] = true
		}
		if row.Changes == nil {
			row.Changes = &proto.StorageCategory{}
		}
		row.Changes.Items += stats.Items
		row.Changes.Bytes += stats.Bytes
		row.TotalBytes += stats.Bytes
	}
	for _, result := range results {
		isLocal := strings.HasPrefix(result.target.url, "unix://")
		var row *dfRow
		if isLocal {
			identity := localStorageTargetIdentity(result.target.url)
			if seenTargets[identity] {
				continue
			}
			seenTargets[identity] = true
			row = localRow()
		} else {
			rows = append(rows, dfRow{Location: result.target.name})
			row = &rows[len(rows)-1]
		}
		row.hasRunner = true
		stats := result.value
		if stats.Cache != nil {
			if row.Cache == nil {
				copied := *stats.Cache
				row.Cache = &copied
			} else {
				row.Cache.Bytes += stats.Cache.Bytes
				row.Cache.Blobs += stats.Cache.Blobs
				row.Cache.MaxBytes += stats.Cache.MaxBytes
				if row.Cache.TTLHours != stats.Cache.TTLHours {
					row.Cache.TTLHours = 0
				}
			}
			row.TotalBytes += stats.Cache.Bytes
		}
		if stats.NamedCaches != nil {
			if row.NamedCaches == nil {
				row.NamedCaches = &proto.NamedCacheStats{}
			}
			row.NamedCaches.Bytes += stats.NamedCaches.Bytes
			row.NamedCaches.Items += stats.NamedCaches.Items
			row.NamedCaches.Protected += stats.NamedCaches.Protected
			row.TotalBytes += stats.NamedCaches.Bytes
		}
		row.Jobs.Bytes += stats.Jobs.Bytes
		row.Jobs.Items += stats.Jobs.Items
		row.TotalBytes += stats.Jobs.Bytes
		addChanges(row, stats.Changes, isLocal)
	}
	if changes != nil {
		addChanges(localRow(), changes, true)
	}
	return rows
}

// Socket aliases must not multiply a daemon's totals. Keep this identity local
// to inventory: persisted job URLs must retain their original transport path.
func localStorageTargetIdentity(target string) string {
	u, err := url.Parse(target)
	if err != nil {
		return target
	}
	path, err := hex.DecodeString(u.Host)
	if err != nil {
		return target
	}
	info, err := os.Stat(string(path)) // Follow socket and parent-directory symlinks.
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return target
	}
	identity, err := fsidentity.FromInfo(info)
	if err != nil {
		return target
	}
	return fmt.Sprintf("socket:%d:%d", identity.Device, identity.Inode)
}
