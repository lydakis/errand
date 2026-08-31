package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	outputops "github.com/lydakis/errand/internal/outputs"
	"github.com/lydakis/errand/internal/proto"
)

func downloadOutputBundle(peerURL, jobID string, expected proto.OutputSummary) (string, proto.OutputBundle, error) {
	var bundle proto.OutputBundle
	root, err := localOutputRoot()
	if err != nil {
		return "", bundle, err
	}
	downloads := filepath.Join(root, "downloads")
	if err := ensurePrivateLocalDirectory(root); err != nil {
		return "", bundle, err
	}
	if err := ensurePrivateLocalDirectory(downloads); err != nil {
		return "", bundle, err
	}
	key := localOutputKey(peerURL, jobID)
	dest := filepath.Join(downloads, key)
	unlock, err := acquireLocalOutputLock(localOutputTransferLockName(key))
	if err != nil {
		return "", bundle, err
	}
	defer unlock()
	if err := removeLocalDownloadTemps(downloads, key); err != nil {
		return "", bundle, err
	}
	if existing, err := loadStagedBundle(dest); err == nil {
		if outputSummaryMatches(existing, expected) {
			now := time.Now()
			if err := os.Chtimes(dest, now, now); err != nil {
				return "", bundle, fmt.Errorf("refreshing staged outputs: %w", err)
			}
			return dest, existing, nil
		}
		if err := os.RemoveAll(dest); err != nil {
			return "", bundle, fmt.Errorf("replacing stale staged outputs: %w", err)
		}
	} else {
		replace := !errors.Is(err, os.ErrNotExist)
		if !replace {
			if _, statErr := os.Lstat(dest); statErr == nil {
				replace = true
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return "", bundle, fmt.Errorf("inspecting staged outputs: %w", statErr)
			}
		}
		if replace {
			if err := os.RemoveAll(dest); err != nil {
				return "", bundle, fmt.Errorf("replacing corrupt staged outputs: %w", err)
			}
		}
	}

	req, err := http.NewRequest(http.MethodGet,
		strings.TrimSuffix(peerURL, "/")+"/v0/jobs/"+jobID+"/outputs", nil)
	if err != nil {
		return "", bundle, err
	}
	resp, err := maintenanceHTTP.Do(req)
	if err != nil {
		return "", bundle, err
	}
	body := &idleReadCloser{ReadCloser: resp.Body, timeout: streamIdleTimeout}
	defer body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(body, 1<<20))
		return "", bundle, fmt.Errorf("fetching outputs: %s: %s", resp.Status, apiError(raw))
	}
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return "", bundle, fmt.Errorf("fetching outputs: invalid multipart response")
	}
	mr := multipart.NewReader(body, params["boundary"])
	bundlePart, err := mr.NextPart()
	if err != nil || bundlePart.FormName() != "bundle" {
		return "", bundle, fmt.Errorf("fetching outputs: missing bundle metadata")
	}
	bundleRaw, err := readBoundedBody(bundlePart, outputops.MaxBundleMetadataBytes, "output bundle")
	if err != nil {
		return "", bundle, fmt.Errorf("fetching outputs: %w", err)
	}
	if err := json.Unmarshal(bundleRaw, &bundle); err != nil {
		return "", bundle, fmt.Errorf("fetching outputs: decoding bundle: %w", err)
	}
	if err := outputops.ValidateBundle(bundle); err != nil {
		return "", bundle, fmt.Errorf("fetching outputs: %w", err)
	}
	if !outputSummaryMatches(bundle, expected) {
		return "", bundle, fmt.Errorf("fetching outputs: bundle does not match the terminal receipt")
	}
	archivePart, err := mr.NextPart()
	if err != nil || archivePart.FormName() != "archive" {
		return "", bundle, fmt.Errorf("fetching outputs: missing archive")
	}
	tmp, err := os.MkdirTemp(downloads, ".outputs-"+key+"-")
	if err != nil {
		return "", bundle, err
	}
	defer os.RemoveAll(tmp)
	workspace := filepath.Join(tmp, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		return "", bundle, err
	}
	archiveLimit, err := outputops.ArchiveByteLimit(bundle.Bytes)
	if err != nil {
		return "", bundle, err
	}
	if err := outputops.Extract(io.LimitReader(archivePart, archiveLimit), workspace, bundle, proto.DefaultLimits().MaxOutputBytes); err != nil {
		return "", bundle, fmt.Errorf("fetching outputs: extracting archive: %w", err)
	}
	if err := writeLocalJSON(filepath.Join(tmp, "bundle.json"), bundle); err != nil {
		return "", bundle, err
	}
	if err := syncLocalDirectory(tmp); err != nil {
		return "", bundle, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", bundle, err
	}
	if err := syncLocalDirectory(downloads); err != nil {
		return "", bundle, err
	}
	return dest, bundle, nil
}

func loadStagedBundle(dir string) (proto.OutputBundle, error) {
	var bundle proto.OutputBundle
	f, err := os.Open(filepath.Join(dir, "bundle.json"))
	if err != nil {
		return bundle, err
	}
	defer f.Close()
	raw, err := readBoundedBody(f, outputops.MaxBundleMetadataBytes, "staged output bundle")
	if err != nil {
		return bundle, err
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return bundle, err
	}
	if err := outputops.ValidateBundle(bundle); err != nil {
		return bundle, err
	}
	if err := outputops.VerifyExtracted(filepath.Join(dir, "workspace"), bundle); err != nil {
		return bundle, err
	}
	return bundle, nil
}

func outputSummaryMatches(bundle proto.OutputBundle, summary proto.OutputSummary) bool {
	if bundle.Bytes != summary.Bytes || bundle.Manifest.RootHash() != summary.ManifestRoot || len(bundle.Paths) != len(summary.Paths) {
		return false
	}
	for i := range bundle.Paths {
		if bundle.Paths[i] != summary.Paths[i] {
			return false
		}
	}
	return true
}

func removeLocalDownloadTemps(downloads, key string) error {
	entries, err := os.ReadDir(downloads)
	if err != nil {
		return err
	}
	prefix := ".outputs-" + key + "-"
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			if err := os.RemoveAll(filepath.Join(downloads, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
