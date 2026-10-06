package cloud

import (
	"archive/tar"
	"bytes"
	"context"
	"debug/buildinfo"
	"debug/elf"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/lydakis/errand/internal/limitbuf"
	"github.com/lydakis/errand/internal/nowindow"
)

//go:embed lambda_install.sh
var lambdaInstallScriptFile string

// lambdaInstallScript is the script with Unix line ends, whatever a Windows
// checkout made of them.
var lambdaInstallScript = strings.ReplaceAll(lambdaInstallScriptFile, "\r\n", "\n")

// lambdaInstallCommand unpacks the install bundle from stdin into a fresh
// directory and runs its script as root. The directory holds the auth key,
// so it is removed however this ends, also when sudo cannot run the script.
// It is the only command run with the bundle, and it never changes.
const lambdaInstallCommand = `d="$HOME/.errand-lease"; rm -rf "$d" && mkdir -m 700 "$d" || exit 1; tar -xf - -C "$d" && sudo bash "$d/install.sh"; s=$?; rm -rf "$d"; exit $s`

// install copies errand, its runner config and either the tailnet auth key
// or the client's SSH key to the machine as one archive over SSH stdin and
// runs a fixed script from it. Values travel only as file contents, so none
// needs quoting for a shell or systemd, and the auth key never appears in
// launch metadata or a command. binary is the errand build checkInstall
// checked.
func (p *LambdaProvider) install(ctx context.Context, binary *os.File, ip, hostPublic, hostname, authKey, login, clientKey string) error {
	known, err := os.CreateTemp("", "errand-lease-known-hosts-")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(known, "%s %s\n", ip, hostPublic)
	if closeErr := known.Close(); err == nil {
		err = closeErr
	}
	defer os.Remove(known.Name())
	if err != nil {
		return err
	}
	user := lambdaUser
	base := []string{
		"-i", p.keyFile(),
		"-o", "BatchMode=yes",
		"-o", "IdentitiesOnly=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "HostKeyAlgorithms=ssh-ed25519",
		"-o", "UserKnownHostsFile=" + known.Name(),
		"-o", "GlobalKnownHostsFile=/dev/null",
		"--", user + "@" + ip,
	}
	run := func(stdin io.Reader, command string) ([]byte, error) {
		return p.ssh()(ctx, append(slices.Clone(base), command), stdin)
	}
	// sshd usually answers a little after the instance turns active, and may
	// show the image's own host key until cloud-init installs the pinned one.
	// Both are waited out; strict checking means nothing is sent to a host
	// that has not shown the pinned key.
	for {
		out, err := run(nil, "true")
		if err == nil {
			break
		}
		if detail := lastLine(out); sshPermanent(detail) {
			return fmt.Errorf("SSH to %s@%s failed: %s", user, ip, detail)
		}
		if sleepErr := sleep(ctx, p.poll()); sleepErr != nil {
			return fmt.Errorf("SSH to %s did not answer: %v %s", ip, err, lastLine(out))
		}
	}
	allow := p.allowUsers()
	if login != "" && !slices.Contains(allow, login) {
		allow = append(allow, login)
	}
	config, err := lambdaRunnerConfig(authKey != "", allow)
	if err != nil {
		return err
	}
	bundle, w := io.Pipe()
	wrote := make(chan error, 1)
	go func() {
		err := writeInstallBundle(w, binary, config, hostname, authKey, clientKey)
		w.CloseWithError(err)
		wrote <- err
	}()
	out, err := run(bundle, lambdaInstallCommand)
	bundle.Close() // unblocks the writer if ssh stopped reading
	// A closed pipe only means ssh failed first, which err reports.
	if bundleErr := <-wrote; bundleErr != nil && !errors.Is(bundleErr, io.ErrClosedPipe) {
		return fmt.Errorf("sending errand to %s: %w", ip, bundleErr)
	}
	if err != nil {
		return fmt.Errorf("installing errand on %s: %v %s", ip, err, lastLine(out))
	}
	return nil
}

// lambdaRunnerConfig is the errandd.toml of a leased machine's runner: on
// the tailnet it admits allowUsers and refuses SSH clients, so the tailnet
// policy decides who gets in; otherwise it is reached only over SSH.
func lambdaRunnerConfig(tailnet bool, allowUsers []string) ([]byte, error) {
	var b bytes.Buffer
	if !tailnet {
		err := toml.NewEncoder(&b).Encode(struct {
			Transport string `toml:"transport"`
		}{"ssh"})
		return b.Bytes(), err
	}
	err := toml.NewEncoder(&b).Encode(struct {
		Transport  string   `toml:"transport"`
		Listen     string   `toml:"listen"`
		AllowUsers []string `toml:"allow_users"`
	}{"tailscale", "tailnet:7443", allowUsers})
	return b.Bytes(), err
}

// writeInstallBundle writes the archive lambdaInstallCommand unpacks.
// Over SSH the machine admits clientKey; on the tailnet it needs no key.
func writeInstallBundle(w io.Writer, binary *os.File, config []byte, hostname, authKey, clientKey string) error {
	tw := tar.NewWriter(w)
	add := func(name string, mode int64, size int64, body io.Reader) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: size, ModTime: time.Unix(0, 0), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		_, err := io.Copy(tw, body)
		return err
	}
	addString := func(name string, mode int64, body string) error {
		return add(name, mode, int64(len(body)), strings.NewReader(body))
	}
	if err := addString("install.sh", 0o600, lambdaInstallScript); err != nil {
		return err
	}
	if authKey != "" {
		if err := addString("tailscale-auth-key", 0o600, authKey+"\n"); err != nil {
			return err
		}
	}
	if authKey == "" {
		if err := addString("authorized-keys", 0o644, clientKey+"\n"); err != nil {
			return err
		}
	}
	if err := addString("hostname", 0o644, hostname+"\n"); err != nil {
		return err
	}
	if err := add("errandd.toml", 0o644, int64(len(config)), bytes.NewReader(config)); err != nil {
		return err
	}
	info, err := binary.Stat()
	if err != nil {
		return err
	}
	if err := add("errand", 0o755, info.Size(), io.NewSectionReader(binary, 0, info.Size())); err != nil {
		return fmt.Errorf("errand_binary: %w", err)
	}
	return tw.Close()
}

// checkInstall checks what the install needs before anything is rented. It
// returns a private copy of errand_binary, which is what it checked and what
// the install sends: the file at errand_binary may be replaced in between.
// The caller closes and removes the copy.
func (p *LambdaProvider) checkInstall() (*os.File, error) {
	if p.HostKey == nil || p.Keygen == nil {
		if _, err := exec.LookPath("ssh-keygen"); err != nil {
			return nil, fmt.Errorf("renting Lambda machines needs ssh-keygen: %w", err)
		}
	}
	if p.SSH == nil {
		if _, err := exec.LookPath("ssh"); err != nil {
			return nil, fmt.Errorf("installing errand on Lambda machines needs ssh: %w", err)
		}
	}
	binary, err := privateCopy(p.ErrandBinary)
	if err == nil {
		if err = checkLinuxBinary(binary, p.ErrandBinary, p.Arch); err != nil {
			removeCopy(binary)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("lambda errand_binary: %w", err)
	}
	return binary, nil
}

// privateCopy copies the regular file at path to a temporary file only this
// user can read, and returns it open.
func privateCopy(path string) (*os.File, error) {
	// Opening a FIFO would block, so the type is checked before opening
	// and again on what was opened.
	if info, err := os.Stat(path); err != nil {
		return nil, err
	} else if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	src, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	if info, err := src.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	dst, err := os.CreateTemp("", "errand-lease-binary-")
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(dst, src); err != nil {
		removeCopy(dst)
		return nil, fmt.Errorf("copying %s: %w", path, err)
	}
	return dst, nil
}

func removeCopy(f *os.File) {
	f.Close()
	os.Remove(f.Name())
}

func (p *LambdaProvider) hostKey() func(context.Context) (string, string, error) {
	if p.HostKey != nil {
		return p.HostKey
	}
	return func(ctx context.Context) (string, string, error) {
		dir, err := os.MkdirTemp("", "errand-lease-host-key-")
		if err != nil {
			return "", "", err
		}
		defer os.RemoveAll(dir)
		path := dir + "/key"
		cmd := exec.CommandContext(ctx, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "errand-lease", "-f", path)
		nowindow.Hide(cmd)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", "", fmt.Errorf("ssh-keygen: %v %s", err, lastLine(out))
		}
		private, err := os.ReadFile(path)
		if err != nil {
			return "", "", err
		}
		public, err := os.ReadFile(path + ".pub")
		if err != nil {
			return "", "", err
		}
		return strings.ReplaceAll(string(private), "\r\n", "\n"), strings.TrimSpace(string(public)), nil
	}
}

// hostKeyCloudConfig has cloud-init install the given ed25519 host key and
// drop the image's own keys.
func hostKeyCloudConfig(private, public string) string {
	var b strings.Builder
	b.WriteString("#cloud-config\nssh_deletekeys: true\nssh_genkeytypes: []\nssh_keys:\n  ed25519_private: |\n")
	for _, line := range strings.Split(strings.TrimSpace(private), "\n") {
		b.WriteString("    " + line + "\n")
	}
	b.WriteString("  ed25519_public: " + public + "\n")
	return b.String()
}

// checkLinuxBinary makes sure f, read from path, is a Linux executable for
// arch, so a wrong build fails here instead of on a paid machine.
func checkLinuxBinary(f io.ReaderAt, path, arch string) error {
	bin, err := elf.NewFile(f)
	if err != nil {
		return fmt.Errorf("%s is not a Linux executable", path)
	}
	want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[arch]
	if bin.Type != elf.ET_EXEC && bin.Type != elf.ET_DYN || bin.Machine != want {
		return fmt.Errorf("%s is a %s %s file, not a linux/%s executable", path, bin.Machine, bin.Type, arch)
	}
	// The ELF header alone also fits a BSD build or another program; Go's
	// build info says which program it is and which OS it was built for.
	info, err := buildinfo.Read(f)
	if err != nil || info.Path != errandMainPackage {
		return fmt.Errorf("%s is not an errand build", path)
	}
	for _, s := range info.Settings {
		if s.Key == "GOOS" && s.Value != "linux" {
			return fmt.Errorf("%s is errand for %s, not linux", path, s.Value)
		}
	}
	return nil
}

const errandMainPackage = "github.com/lydakis/errand/cmd/errand"

// sshPermanent recognizes SSH failures that retrying cannot fix: a key the
// instance rejects or an unreadable key. A host key other than the pinned one
// is not among them, since cloud-init may not have installed it yet.
func sshPermanent(detail string) bool {
	for _, s := range []string{"Permission denied", "Load key", "no such identity", "Bad configuration option"} {
		if strings.Contains(detail, s) {
			return true
		}
	}
	return false
}

func (p *LambdaProvider) ssh() func(context.Context, []string, io.Reader) ([]byte, error) {
	if p.SSH != nil {
		return p.SSH
	}
	return func(ctx context.Context, args []string, stdin io.Reader) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "ssh", args...)
		cmd.Stdin = stdin
		cmd.WaitDelay = 5 * time.Second
		nowindow.Hide(cmd)
		out := &limitbuf.Buffer{Limit: 4096}
		cmd.Stdout, cmd.Stderr = out, out
		err := cmd.Run()
		return out.Bytes(), err
	}
}
