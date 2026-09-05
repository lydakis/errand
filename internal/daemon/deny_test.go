package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lydakis/errand/internal/proto"
	"github.com/lydakis/errand/internal/tailnet"
)

type denyIdentityProvider struct {
	tailnet.Provider
	identity tailnet.WhoIs
}

func (p denyIdentityProvider) WhoIs(context.Context, string, string) (tailnet.WhoIs, error) {
	return p.identity, nil
}

func TestDenyUsersPrecedesTailnetGrants(t *testing.T) {
	const login = "friend@example.com"
	for _, tc := range []struct {
		name                       string
		login                      string
		deny                       []string
		allow, capability, refused bool
	}{
		{"allowlist", login, []string{login}, true, false, true},
		{"capability", login, []string{login}, false, true, true},
		{"both", login, []string{login}, true, true, true},
		{"different login", login, []string{"other@example.com"}, true, true, false},
		{"no denials", login, nil, false, true, false},
		{"tagged node", "", []string{"", login}, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			who := tailnet.WhoIs{LoginName: tc.login, UserID: 42, NodeStableID: "node-1"}
			if tc.capability {
				who.CapMap = map[string][]json.RawMessage{proto.DefaultCapability: {json.RawMessage(`{"actions":["*"]}`)}}
			}
			d := &Daemon{cfg: Config{DenyUsers: tc.deny, Capability: proto.DefaultCapability}, identity: denyIdentityProvider{identity: who}}
			if tc.allow {
				d.cfg.AllowUsers = []string{login}
			}
			id, err := d.identify("100.64.0.1:1234", testDestination())
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "denied") || len(id.Actions) != 0 {
					t.Fatalf("denied caller acquired authorization: %+v, %v", id, err)
				}
			} else if err != nil || !id.Allowed(proto.ActionSubmit) {
				t.Fatalf("unaffected caller refused: %+v, %v", id, err)
			}
		})
	}
}

func TestTailnetDenialDoesNotChangeLocalAuthorization(t *testing.T) {
	d := &Daemon{cfg: Config{DenyUsers: []string{"runner"}}, selfUID: 1000}
	for _, uid := range []uint32{1000, 1001} {
		id, err := d.identifyLocal(LocalPeer{UID: uid, User: "runner"})
		if uid == d.selfUID {
			if err != nil || !id.Allowed(proto.ActionSubmit) {
				t.Fatalf("local owner refused: %+v, %v", id, err)
			}
		} else if err == nil || id.Allowed(proto.ActionSubmit) {
			t.Fatalf("other local user authorized: %+v, %v", id, err)
		}
	}
}
