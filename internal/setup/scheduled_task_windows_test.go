//go:build windows

package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lydakis/errand/internal/proto"
)

// Compare against Task Scheduler's actual export, including the defaults it
// inserts. Register a unique task without starting it or touching errand's task.
func TestScheduledTaskExportMatchesSetupOnWindows(t *testing.T) {
	sys := RealSystem{}
	user, err := sys.UserSID()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dir = filepath.Join(dir, "task with spaces 世界")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	definition, err := decodeUTF16(renderScheduledTask(user,
		filepath.Join(dir, "runtime", "generation", "errand.exe"), filepath.Join(dir, "config.toml"), filepath.Join(dir, "log.txt")))
	if err != nil {
		t.Fatal(err)
	}
	name := "errand-test-" + proto.NewULID()
	definition = strings.ReplaceAll(definition, `<URI>\errand</URI>`, `<URI>\`+name+`</URI>`)
	file := filepath.Join(dir, "task.xml")
	if err := os.WriteFile(file, []byte(encodeUTF16(definition)), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	out, err := sys.Run(ctx, "schtasks", "/Create", "/TN", name, "/XML", file, "/F")
	if err != nil {
		t.Fatalf("register task: %v: %s", err, out)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if out, err := sys.Run(ctx, "schtasks", "/Delete", "/TN", name, "/F"); err != nil {
			t.Errorf("remove task: %v: %s", err, out)
		}
	})
	query := strings.Replace(scheduledTaskDefinitionQuery, "-TaskName '"+scheduledTaskName+"'", "-TaskName '"+name+"'", 1)
	registered, err := readScheduledTask(ctx, sys, query)
	if err != nil {
		t.Fatalf("query task: %v", err)
	}
	if registered.State != "Ready" || registered.Command != filepath.Join(dir, "runtime", "generation", "errand.exe") {
		t.Fatalf("registered task state/command = %q / %q", registered.State, registered.Command)
	}
	if !sameTaskDefinition(registered.Definition, strings.ReplaceAll(definition, "\r\n", "\n")) {
		actual, _ := taskDefinitionFields(registered.Definition)
		expected, _ := taskDefinitionFields(definition)
		t.Fatalf("registered definition differs from setup:\nactual: %v\nexpected: %v", actual, expected)
	}
}
