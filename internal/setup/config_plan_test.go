package setup

import (
	"context"
	"strings"
	"testing"
)

func TestSetupRejectsInvalidPlanBeforeReservingRunner(t *testing.T) {
	f := newFake(t, "linux")
	path := f.home + "/.config/errand/errandd.toml"
	original := "transport = 'both'\nlisten = 'tailnet:7443'\nmax_jobs = 0\n"
	f.files[path] = original

	r, err := Run(context.Background(), Options{Transport: "ssh"}, f)
	if err != nil || !r.Failed() || !strings.Contains(stepErrorDetail(r, "config"), "max_jobs must be positive") {
		t.Fatalf("invalid plan was not rejected: %v / %+v", err, r)
	}
	if len(f.quiesceSockets) != 0 || len(f.writes) != 0 || len(f.commands) != 0 || f.files[path] != original {
		t.Fatal("invalid plan changed the config or reserved/restarted the runner")
	}
}

type configEditingSystem struct {
	*fakeSystem
	configPath string
	edited     string
}

func (s configEditingSystem) Quiesce(ctx context.Context, socket string) (string, error) {
	token, err := s.fakeSystem.Quiesce(ctx, socket)
	s.files[s.configPath] = s.edited
	return token, err
}

func TestSetupPreservesConfigEditedAfterPlanning(t *testing.T) {
	f := newFake(t, "linux")
	path := f.home + "/.config/errand/errandd.toml"
	original := "transport = 'both'\nlisten = 'tailnet:7443'\nmax_jobs = 1\n"
	f.files[path] = original
	sys := configEditingSystem{fakeSystem: f, configPath: path, edited: original + "deny_users = ['revoked@example.com']\n"}

	r, err := Run(context.Background(), Options{Transport: "ssh"}, sys)
	if err != nil || !r.Failed() || !strings.Contains(stepErrorDetail(r, "config"), "config changed during setup") {
		t.Fatalf("concurrent edit was not detected: %v / %+v", err, r)
	}
	if f.files[path] != sys.edited || len(f.writes) != 0 || len(f.commands) != 0 {
		t.Fatal("setup overwrote the operator's edit or changed the service")
	}
	if len(f.releasedLeases) != 1 {
		t.Fatalf("setup left the runner reserved: released leases %v", f.releasedLeases)
	}
}
