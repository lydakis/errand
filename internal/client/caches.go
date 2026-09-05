package client

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/lydakis/errand/internal/fsidentity"
	"github.com/lydakis/errand/internal/proto"
)

// cacheProjectID persists a private identity for this checkout, independent
// of source contents and project labels. Replacing the directory gets a new ID.
func cacheProjectID(workspace string) (string, error) {
	root, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", err
	}
	identity, info, err := fsidentity.Lstat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cache workspace must be a directory")
	}
	state, err := localChangeRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(state, "cache-projects")
	if err := ensurePrivateLocalDirectory(dir); err != nil {
		return "", err
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	key := fmt.Sprintf("%x", sha256.Sum256(raw))
	unlock, err := acquireLocalChangeLock("cache-project-" + key)
	if err != nil {
		return "", err
	}
	defer unlock()
	dest := filepath.Join(dir, key+".json")
	var id string
	if raw, err := os.ReadFile(dest); err == nil {
		if err := json.Unmarshal(raw, &id); err != nil || !proto.ValidChangeClientID(id) {
			return "", fmt.Errorf("invalid cache project identity")
		}
		return id, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	id = hex.EncodeToString(random)
	tmp := filepath.Join(dir, ".project-"+proto.NewULID())
	defer os.Remove(tmp)
	if err := writeLocalJSON(tmp, id); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	if err := syncLocalDirectory(dir); err != nil {
		return "", err
	}
	return id, nil
}
