package daemon

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/lydakis/errand/internal/client"
)

func writeCacheTestTarball(t *testing.T, root, version string, value int) {
	t.Helper()
	f, err := os.Create(filepath.Join(root, "tiny.tgz"))
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"package/package.json": fmt.Sprintf(`{"name":"tiny","version":%q,"main":"index.js"}`, version), "package/index.js": fmt.Sprintf("module.exports = %d;\n", value)} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, err := range []error{tw.Close(), gz.Close(), f.Close()} {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func writeCacheTestWheel(t *testing.T, root, version string, value int) {
	t.Helper()
	f, err := os.Create(filepath.Join(root, "tiny-"+version+"-py3-none-any.whl"))
	if err != nil {
		t.Fatal(err)
	}
	z := zip.NewWriter(f)
	info := "tiny-" + version + ".dist-info/"
	for name, body := range map[string]string{"tiny/__init__.py": fmt.Sprintf("value = %d\ndef main():\n    print(value)\n", value), info + "METADATA": "Metadata-Version: 2.1\nName: tiny\nVersion: " + version + "\n", info + "WHEEL": "Wheel-Version: 1.0\nGenerator: errand-test\nRoot-Is-Purelib: true\nTag: py3-none-any\n", info + "RECORD": "", info + "entry_points.txt": "[console_scripts]\ntiny-cli = tiny:main\n"} {
		w, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func runConcurrentCacheCommands(t *testing.T, options []client.RunOptions, argv []string) {
	t.Helper()
	results := make(chan string, len(options))
	for _, opts := range options {
		go func(opts client.RunOptions) {
			var out bytes.Buffer
			opts.Argv, opts.Stdout, opts.Stderr = argv, &out, &out
			code := client.Run(opts)
			if code != 0 {
				results <- fmt.Sprintf("%s: %d %s", opts.Workspace, code, &out)
			} else {
				results <- ""
			}
		}(opts)
	}
	for range options {
		if failure := <-results; failure != "" {
			t.Error(failure)
		}
	}
}
