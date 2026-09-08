package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lydakis/errand/internal/proto"
)

func TestLocalOnlyRejectsNetworkEvenWithAnAuthorizedIdentity(t *testing.T) {
	d, local := unixDaemon(t, Config{LocalOnly: true, InsecureNoAuth: true})
	remote := httptest.NewServer(d.Handler())
	defer remote.Close()
	for _, path := range []string{"/v0/info", "/v0/jobs"} {
		res, err := http.Get(remote.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Fatalf("network %s = %s", path, res.Status)
		}
		res, err = local.Get("http://errand" + path)
		if err != nil {
			t.Fatal(err)
		}
		if path == "/v0/info" {
			var info proto.Info
			if err := json.NewDecoder(res.Body).Decode(&info); err != nil {
				t.Fatal(err)
			}
			if !info.SSHDisabled || !info.LocalOnly {
				t.Fatal("SSH bridge not disabled")
			}
		}
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("local %s = %s", path, res.Status)
		}
	}
}
