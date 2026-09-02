package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	changeops "github.com/lydakis/errand/internal/changes"
	"github.com/lydakis/errand/internal/proto"
)

func downloadChangeBundle(peerURL, jobID string, expected proto.ChangeSummary) (string, proto.ChangeBundle, error) {
	var bundle proto.ChangeBundle
	key := localChangeKey(peerURL, jobID)
	unlock, err := acquireLocalChangeLock(localChangeTransferLockName(key))
	if err != nil {
		return "", bundle, err
	}
	defer unlock()
	return downloadChangeBundleLocked(peerURL, jobID, key, expected)
}

func downloadChangeBundleLocked(peerURL, jobID, key string, expected proto.ChangeSummary) (string, proto.ChangeBundle, error) {
	var bundle proto.ChangeBundle
	root, err := localChangeRoot()
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
	dest := filepath.Join(downloads, key)
	if err := removeLocalDownloadTemps(downloads, key); err != nil {
		return "", bundle, err
	}
	if existing, err := loadStagedBundle(dest); err == nil {
		if expected.Matches(existing) {
			now := time.Now()
			if err := os.Chtimes(dest, now, now); err != nil {
				return "", bundle, fmt.Errorf("refreshing staged changes: %w", err)
			}
			return dest, existing, nil
		}
		if err := changeops.RemoveTree(dest); err != nil {
			return "", bundle, fmt.Errorf("replacing stale staged changes: %w", err)
		}
	} else {
		replace := !errors.Is(err, os.ErrNotExist)
		if !replace {
			if _, statErr := os.Lstat(dest); statErr == nil {
				replace = true
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return "", bundle, fmt.Errorf("inspecting staged changes: %w", statErr)
			}
		}
		if replace {
			if err := changeops.RemoveTree(dest); err != nil {
				return "", bundle, fmt.Errorf("replacing corrupt staged changes: %w", err)
			}
		}
	}

	req, err := http.NewRequest(http.MethodGet,
		strings.TrimSuffix(peerURL, "/")+"/v0/jobs/"+jobID+"/changes", nil)
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
		return "", bundle, fmt.Errorf("fetching changes: %s: %s", resp.Status, apiError(raw))
	}
	mediaType, params, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return "", bundle, fmt.Errorf("fetching changes: invalid multipart response")
	}
	responseLimit, err := changeResponseByteLimit(expected.Bytes)
	if err != nil {
		return "", bundle, fmt.Errorf("fetching changes: %w", err)
	}
	boundedBody := &hardLimitReader{reader: body, remaining: responseLimit}
	mr := multipart.NewReader(boundedBody, params["boundary"])
	bundlePart, err := mr.NextPart()
	if err != nil || bundlePart.FormName() != "bundle" {
		return "", bundle, fmt.Errorf("fetching changes: missing bundle metadata")
	}
	bundleRaw, err := readBoundedBody(bundlePart, changeops.MaxBundleMetadataBytes, "change bundle")
	if err != nil {
		return "", bundle, fmt.Errorf("fetching changes: %w", err)
	}
	if err := json.Unmarshal(bundleRaw, &bundle); err != nil {
		return "", bundle, fmt.Errorf("fetching changes: decoding bundle: %w", err)
	}
	if err := changeops.ValidateBundle(bundle); err != nil {
		return "", bundle, fmt.Errorf("fetching changes: %w", err)
	}
	if !expected.Matches(bundle) {
		return "", bundle, fmt.Errorf("fetching changes: bundle does not match the terminal receipt")
	}
	tmp, err := os.MkdirTemp(downloads, ".changes-"+key+"-")
	if err != nil {
		return "", bundle, err
	}
	defer changeops.RemoveTree(tmp)
	base := filepath.Join(tmp, "base")
	if err := os.Mkdir(base, 0o700); err != nil {
		return "", bundle, err
	}
	remote := filepath.Join(tmp, "remote")
	if err := os.Mkdir(remote, 0o700); err != nil {
		return "", bundle, err
	}
	archiveLimit, err := changeops.ArchiveByteLimit(bundle.Bytes)
	if err != nil {
		return "", bundle, err
	}
	basePart, err := mr.NextPart()
	if err != nil || basePart.FormName() != "base" {
		return "", bundle, fmt.Errorf("fetching changes: missing base archive")
	}
	if err := consumeChangeArchive(basePart, archiveLimit, func(reader io.Reader) error {
		return changeops.ExtractBase(reader, base, bundle, proto.DefaultLimits().MaxChangeBytes)
	}); err != nil {
		return "", bundle, fmt.Errorf("fetching changes: extracting base archive: %w", err)
	}
	remotePart, err := mr.NextPart()
	if err != nil || remotePart.FormName() != "remote" {
		return "", bundle, fmt.Errorf("fetching changes: missing remote archive")
	}
	if err := consumeChangeArchive(remotePart, archiveLimit, func(reader io.Reader) error {
		return changeops.ExtractRemote(reader, remote, bundle, proto.DefaultLimits().MaxChangeBytes)
	}); err != nil {
		return "", bundle, fmt.Errorf("fetching changes: extracting remote archive: %w", err)
	}
	if extra, err := mr.NextPart(); err == nil {
		extra.Close()
		return "", bundle, fmt.Errorf("fetching changes: unexpected multipart part")
	} else if !errors.Is(err, io.EOF) {
		return "", bundle, fmt.Errorf("fetching changes: completing multipart response: %w", err)
	}
	if _, err := io.Copy(io.Discard, boundedBody); err != nil {
		return "", bundle, fmt.Errorf("fetching changes: completing response: %w", err)
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

var errChangeResponseTooLarge = errors.New("change response exceeds bounded size")

type hardLimitReader struct {
	reader    io.Reader
	remaining int64
}

func (r *hardLimitReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		var probe [1]byte
		n, err := r.reader.Read(probe[:])
		if n != 0 {
			return 0, errChangeResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.reader.Read(p)
	r.remaining -= int64(n)
	return n, err
}

func changeResponseByteLimit(logicalBytes int64) (int64, error) {
	archiveLimit, err := changeops.ArchiveByteLimit(logicalBytes)
	if err != nil {
		return 0, err
	}
	const framingAllowance = int64(1 << 20)
	metadataAllowance := int64(changeops.MaxBundleMetadataBytes) + framingAllowance
	if archiveLimit > (math.MaxInt64-metadataAllowance)/2 {
		return 0, fmt.Errorf("change response size overflows")
	}
	return 2*archiveLimit + metadataAllowance, nil
}

func consumeChangeArchive(part io.Reader, limit int64, extract func(io.Reader) error) error {
	limited := &io.LimitedReader{R: part, N: limit + 1}
	if err := extract(limited); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return err
	}
	if limited.N == 0 {
		return errChangeResponseTooLarge
	}
	return nil
}

func loadStagedBundle(dir string) (proto.ChangeBundle, error) {
	var bundle proto.ChangeBundle
	f, err := os.Open(filepath.Join(dir, "bundle.json"))
	if err != nil {
		return bundle, err
	}
	defer f.Close()
	raw, err := readBoundedBody(f, changeops.MaxBundleMetadataBytes, "staged change bundle")
	if err != nil {
		return bundle, err
	}
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return bundle, err
	}
	if err := changeops.ValidateBundle(bundle); err != nil {
		return bundle, err
	}
	if err := changeops.VerifyExtracted(dir, bundle); err != nil {
		return bundle, err
	}
	return bundle, nil
}

func removeLocalDownloadTemps(downloads, key string) error {
	entries, err := os.ReadDir(downloads)
	if err != nil {
		return err
	}
	prefix := ".changes-" + key + "-"
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			if err := changeops.RemoveTree(filepath.Join(downloads, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}
