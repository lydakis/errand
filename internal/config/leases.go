package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lydakis/errand/internal/durable"
	"github.com/lydakis/errand/internal/filelock"
	"github.com/lydakis/errand/internal/proto"
)

// LeaseRecord is a client's note of a machine leased from a cloud peer. While
// it exists, the lease is a peer named after the broker and the lease ID, so
// --on, --where and job handles reach the machine like any configured peer.
type LeaseRecord struct {
	Broker string `json:"broker"`
	// BrokerPeer is the cloud peer's entry when the lease was made. Release
	// goes there even if the entry has since changed.
	BrokerPeer Peer              `json:"broker_peer"`
	ID         string            `json:"id"`
	Offer      string            `json:"offer"`
	Target     proto.LeaseTarget `json:"target"`
	CreatedAt  time.Time         `json:"created_at"`
}

// LeasePeer is the peer entry a lease target stands for.
func LeasePeer(t proto.LeaseTarget) Peer {
	return Peer{URL: t.URL, SSH: t.SSH, RemoteCommand: t.RemoteCommand, RemoteSocket: t.RemoteSocket}
}

// StateDirectory holds client state that is not configuration.
func StateDirectory() (string, error) {
	if stateHome := os.Getenv("XDG_STATE_HOME"); stateHome != "" {
		if !filepath.IsAbs(stateHome) {
			return "", fmt.Errorf("XDG_STATE_HOME must be an absolute path")
		}
		return filepath.Join(stateHome, "errand"), nil
	}
	home, err := userHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "errand"), nil
}

func leasesPath() (string, error) {
	dir, err := StateDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "leases.json"), nil
}

// LoadLeases reads the lease records; a missing file means none.
func LoadLeases() (map[string]LeaseRecord, error) {
	path, err := leasesPath()
	if err != nil {
		return nil, err
	}
	return readLeases(path)
}

func readLeases(path string) (map[string]LeaseRecord, error) {
	leases := map[string]LeaseRecord{}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return leases, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &leases); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for name, rec := range leases {
		if validatePeerName(name) != nil || !proto.ValidULID(rec.ID) {
			return nil, fmt.Errorf("%s: invalid lease entry %q", path, name)
		}
	}
	return leases, nil
}

// RecordLease remembers a ready lease and returns its peer name. Recording
// the same lease again returns the name it already has.
func RecordLease(rec LeaseRecord, configured map[string]Peer) (string, error) {
	var name string
	err := updateLeases(func(leases map[string]LeaseRecord) {
		for existing, r := range leases {
			if r.Broker == rec.Broker && r.ID == rec.ID {
				name = existing
				leases[existing] = rec
				return
			}
		}
		id := strings.ToLower(rec.ID)
		for n := 4; n <= len(id); n++ {
			candidate := rec.Broker + "-" + id[len(id)-n:]
			if _, taken := configured[candidate]; taken {
				continue
			}
			if _, taken := leases[candidate]; taken {
				continue
			}
			name = candidate
			break
		}
		leases[name] = rec
	})
	return name, err
}

// ForgetLeases drops the records keep rejects and returns their names.
func ForgetLeases(keep func(name string, rec LeaseRecord) bool) ([]string, error) {
	var dropped []string
	err := updateLeases(func(leases map[string]LeaseRecord) {
		for name, rec := range leases {
			if !keep(name, rec) {
				dropped = append(dropped, name)
				delete(leases, name)
			}
		}
	})
	return dropped, err
}

func updateLeases(change func(map[string]LeaseRecord)) error {
	path, err := leasesPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	// Several agents may lease at once; each holds the lock only briefly.
	for deadline := time.Now().Add(5 * time.Second); ; {
		if err = filelock.TryLock(lock); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("locking %s: %w", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer filelock.Unlock(lock)
	leases, err := readLeases(path)
	if err != nil {
		return err
	}
	change(leases)
	data, err := json.MarshalIndent(leases, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".leases-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := durable.Sync(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
