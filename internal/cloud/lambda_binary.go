package cloud

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// errandReleases is where errand's release archives and their checksums are.
const errandReleases = "https://github.com/lydakis/errand/releases/download"

// lambdaBinaryMu keeps launches from downloading the same build twice.
var lambdaBinaryMu sync.Mutex

func (p *LambdaProvider) useVersion(v string) {
	if p.Version == "" {
		p.Version = v
	}
}

func (p *LambdaProvider) releaseURL() string {
	if p.ReleaseURL != "" {
		return p.ReleaseURL
	}
	return errandReleases
}

// errandBinary is the errand build to install on the machine: errand_binary
// when set, this process when it is a Linux build for the machine's
// architecture, or else this version's release for it, downloaded once into
// the provider's directory and checked against the release's checksums. Any
// of them must be a Linux errand build, so a wrong one fails here instead of
// on a paid machine.
func (p *LambdaProvider) errandBinary(ctx context.Context, progress func(string)) (string, error) {
	if p.ErrandBinary != "" {
		if err := checkLinuxBinary(p.ErrandBinary, p.Arch); err != nil {
			return "", fmt.Errorf("lambda errand_binary: %w", err)
		}
		return p.ErrandBinary, nil
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == p.Arch {
		if self, err := os.Executable(); err == nil && checkLinuxBinary(self, p.Arch) == nil {
			return self, nil
		}
	}
	version := strings.TrimPrefix(p.Version, "v")
	if version == "" || strings.HasSuffix(version, "-dev") {
		return "", fmt.Errorf("this cloud peer runs a development build of errand (%s) on %s/%s, which has no release to install on linux/%s machines; set errand_binary to a linux/%s errand build", p.Version, runtime.GOOS, runtime.GOARCH, p.Arch, p.Arch)
	}
	if p.KeyDir == "" {
		return "", errors.New("lambda provider has no directory for downloaded builds")
	}
	path := filepath.Join(p.KeyDir, "bin", fmt.Sprintf("errand-%s-linux-%s", version, p.Arch))
	lambdaBinaryMu.Lock()
	defer lambdaBinaryMu.Unlock()
	if checkLinuxBinary(path, p.Arch) == nil {
		return path, nil
	}
	progress(fmt.Sprintf("downloading errand %s for linux/%s", version, p.Arch))
	if err := p.downloadErrand(ctx, version, path); err != nil {
		return "", fmt.Errorf("downloading errand %s for linux/%s: %w", version, p.Arch, err)
	}
	if err := checkLinuxBinary(path, p.Arch); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("the errand %s release for linux/%s: %w", version, p.Arch, err)
	}
	return path, nil
}

// downloadErrand fetches the release archive for version and this arch,
// checks it against the release's checksums.txt, and writes the errand
// executable in it to path.
func (p *LambdaProvider) downloadErrand(ctx context.Context, version, path string) error {
	base := p.releaseURL() + "/v" + version + "/"
	asset := fmt.Sprintf("errand_%s_linux_%s.tar.gz", version, p.Arch)
	sums, err := p.fetch(ctx, base+"checksums.txt", 64<<10)
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[1] == asset {
			want = fields[0]
		}
	}
	if want == "" {
		return fmt.Errorf("checksums.txt of release v%s does not list %s", version, asset)
	}
	archive, err := p.fetch(ctx, base+asset, 256<<20)
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(archive); hex.EncodeToString(sum[:]) != want {
		return fmt.Errorf("%s does not match its checksum in checksums.txt", asset)
	}
	gz, err := gzip.NewReader(strings.NewReader(string(archive)))
	if err != nil {
		return fmt.Errorf("%s: %w", asset, err)
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s holds no errand executable", asset)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", asset, err)
		}
		if h.Typeflag != tar.TypeReg || filepath.Base(h.Name) != "errand" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(filepath.Dir(path), ".errand-download-")
		if err != nil {
			return err
		}
		_, err = io.Copy(tmp, io.LimitReader(tr, 256<<20))
		if closeErr := tmp.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Chmod(tmp.Name(), 0o755)
		}
		if err == nil {
			err = os.Rename(tmp.Name(), path)
		}
		if err != nil {
			os.Remove(tmp.Name())
		}
		return err
	}
}

// fetch gets one release file of at most limit bytes.
func (p *LambdaProvider) fetch(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := p.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("GET %s: more than %d bytes", url, limit)
	}
	return data, nil
}
