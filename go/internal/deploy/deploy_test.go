package deploy

import (
	"encoding/json"
	"net/url"
	"testing"
)

func TestTokenURL(t *testing.T) {
	u, err := url.Parse(TokenURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "dash.cloudflare.com" || q.Get("to") != "/:account/api-tokens" || q.Get("name") != "Cloudex" {
		t.Fatalf("unexpected link %s", TokenURL)
	}
	var perms []map[string]string
	if err := json.Unmarshal([]byte(q.Get("permissionGroupKeys")), &perms); err != nil {
		t.Fatal(err)
	}
	if len(perms) != 7 {
		t.Fatalf("got %d permissions, want 7", len(perms))
	}
	for _, p := range perms {
		if len(p["key"]) != 32 || (p["type"] != "edit" && p["type"] != "read") {
			t.Errorf("bad permission %v", p)
		}
	}
}
