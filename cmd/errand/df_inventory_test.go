package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestStorageRowsDeduplicateSocketAliases(t *testing.T) {
	// Keep paths below the Unix socket length limit on macOS.
	dir, err := os.MkdirTemp("/tmp", "errand-df-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "runner.sock")
	other := filepath.Join(dir, "other.sock")
	for _, path := range []string{socket, other} {
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
	}
	alias := filepath.Join(dir, "alias.sock")
	if err := os.Symlink(socket, alias); err != nil {
		t.Fatal(err)
	}
	runner := proto.StorageStats{Jobs: proto.StorageCategory{Items: 1, Bytes: 20}, Cache: &proto.CacheStats{Bytes: 10, MaxBytes: 100}}
	runner.Details = &proto.StorageDetails{Jobs: []proto.JobStorage{{ID: "job", Bytes: 20}}}
	var results []peerQueryResult[proto.StorageStats]
	for _, path := range []string{socket, alias, other} {
		results = append(results, peerQueryResult[proto.StorageStats]{target: peerTarget{url: "unix://" + hex.EncodeToString([]byte(path))}, value: runner})
	}
	rows := storageRows(results[:2], nil)
	if len(rows) != 1 || rows[0].Jobs.Items != 1 || rows[0].TotalBytes != 30 || rows[0].Cache.MaxBytes != 100 || len(rows[0].Details.Jobs) != 1 {
		t.Fatalf("counted socket alias twice: %+v", rows)
	}
	rows = storageRows(results, nil)
	if len(rows) != 1 || rows[0].Jobs.Items != 2 || rows[0].TotalBytes != 60 || rows[0].Cache.MaxBytes != 200 || len(rows[0].Details.Jobs) != 2 {
		t.Fatalf("lost distinct local runner: %+v", rows)
	}
	results[2].value.Details = nil
	rows = storageRows(results, nil)
	if rows[0].Details == nil || !rows[0].Details.Incomplete || len(rows[0].Details.Jobs) != 1 || rows[0].TotalBytes != 60 {
		t.Fatalf("older local runner's missing detail was hidden: %+v", rows)
	}
}

func TestStorageRowsCombineLocalRolesWithoutDoubleCounting(t *testing.T) {
	changes := proto.ChangeStorageStats{StorageCategory: proto.StorageCategory{Items: 2, Bytes: 30}, StoreID: "same-store"}
	runner := proto.StorageStats{Cache: &proto.CacheStats{Bytes: 10}, Jobs: proto.StorageCategory{Items: 1, Bytes: 20}, Changes: &changes}
	results := []peerQueryResult[proto.StorageStats]{
		{target: peerTarget{name: "local", url: "unix://socket"}, value: runner},
		{target: peerTarget{name: "sandbox", url: "unix://socket"}, value: runner},
		{target: peerTarget{name: "cabal", url: "http://cabal:7443"}, value: runner},
	}
	rows := storageRows(results, &changes)
	if len(rows) != 2 || rows[0].Location != "local" || rows[0].TotalBytes != 60 || rows[0].Jobs.Bytes != 20 || rows[0].Changes.Bytes != 30 || rows[1].TotalBytes != 60 {
		t.Fatalf("rows: %+v", rows)
	}
	var out bytes.Buffer
	writeDf(&out, rows)
	if !strings.Contains(out.String(), "20 B") {
		t.Fatalf("local jobs hidden: %s", out.String())
	}
	// Another daemon can use a distinct XDG state directory on the same machine.
	other := changes
	other.StoreID = "other-store"
	other.Bytes = 40
	rows = storageRows(results[:1], &other)
	if len(rows) != 1 || rows[0].Changes.Bytes != 70 || rows[0].TotalBytes != 100 {
		t.Fatalf("lost distinct local store: %+v", rows)
	}
	// An older runner omits changes. Unknown is not treated as a reported zero.
	runner.Changes = nil
	rows = storageRows([]peerQueryResult[proto.StorageStats]{{target: peerTarget{name: "old", url: "http://old"}, value: runner}}, &changes)
	if len(rows) != 2 || rows[0].Changes != nil || rows[0].TotalBytes != 30 || rows[1].Changes.Bytes != 30 {
		t.Fatalf("legacy inventory: %+v", rows)
	}
}

func TestDfShowsClientStorageWithoutConfiguredRunners(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	writeClientConfig(t, "")
	root := filepath.Join(os.Getenv("XDG_STATE_HOME"), "errand", "jobs")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "record.json"), []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := cmdDfTo([]string{"--json"}, &out, &errOut); code != 0 {
		t.Fatalf("df: %d %s", code, errOut.String())
	}
	var rows []dfRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil || len(rows) != 1 || rows[0].Location != "local" || rows[0].Changes == nil || rows[0].Changes.Bytes != 5 || rows[0].TotalBytes != 5 {
		t.Fatalf("local-only inventory: %v %s", err, out.String())
	}
}
