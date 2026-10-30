package runtime_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scanGuardConfig trusts loopback as a proxy so each test can play several
// clients through X-Forwarded-For.
func scanGuardConfig(users, orders *upstream) string {
	return strings.Replace(twoBackendConfig(users, orders), "  listen: \":0\"\n", `  listen: ":0"
  trusted_proxy_cidrs: ["127.0.0.1", "::1"]
  scan_guard:
    max_misses: 3
    window: 1m
    ban: 1m
    # The second pattern covers a served route on purpose.
    trap_paths: ["*/.env*", "/api/users*"]
`, 1)
}

func getAs(t *testing.T, client, method, url string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	req.Header.Set("X-Forwarded-For", client)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestScanGuard(t *testing.T) {
	users := newUpstream(t, usersSpec)
	orders := newUpstream(t, ordersSpec)
	srv, cleanup := buildGateway(t, scanGuardConfig(users, orders))
	defer cleanup()
	gw := httptest.NewServer(srv)
	defer gw.Close()

	t.Run("a trap path is a 404 and then a ban", func(t *testing.T) {
		const c = "203.0.113.1"
		if resp := getAs(t, c, "GET", gw.URL+"/.env"); resp.StatusCode != 404 {
			t.Fatalf("trap hit = %d, want an ordinary 404", resp.StatusCode)
		}
		before := orders.hits.Load()
		resp := getAs(t, c, "GET", gw.URL+"/api/orders")
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("banned client got %d on a real route", resp.StatusCode)
		}
		if resp.Header.Get("Retry-After") == "" {
			t.Error("no Retry-After on a ban")
		}
		if orders.hits.Load() != before {
			t.Error("a banned request reached the upstream")
		}
	})

	t.Run("misses past the threshold ban", func(t *testing.T) {
		const c = "203.0.113.2"
		for i := 0; i < 3; i++ {
			if resp := getAs(t, c, "GET", fmt.Sprintf("%s/nope/%d", gw.URL, i)); resp.StatusCode != 404 {
				t.Fatalf("miss %d = %d", i, resp.StatusCode)
			}
		}
		if resp := getAs(t, c, "GET", gw.URL+"/api/orders"); resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("after the threshold got %d, want 429", resp.StatusCode)
		}
	})

	t.Run("a 405 is not a miss", func(t *testing.T) {
		const c = "203.0.113.3"
		for i := 0; i < 5; i++ {
			if resp := getAs(t, c, "DELETE", gw.URL+"/api/users/abc"); resp.StatusCode != 405 {
				t.Fatalf("got %d, want 405", resp.StatusCode)
			}
		}
		if resp := getAs(t, c, "GET", gw.URL+"/api/orders"); resp.StatusCode != 200 {
			t.Errorf("405s earned a ban: got %d", resp.StatusCode)
		}
	})

	t.Run("a trap pattern never shadows a served route", func(t *testing.T) {
		const c = "203.0.113.4"
		// /api/users matches a trap but is served; banning on it would be an
		// outage of that route.
		for i := 0; i < 2; i++ {
			if resp := getAs(t, c, "GET", gw.URL+"/api/users"); resp.StatusCode != 200 {
				t.Fatalf("request %d: got %d", i, resp.StatusCode)
			}
		}
	})

	t.Run("other clients are unaffected", func(t *testing.T) {
		if resp := getAs(t, "203.0.113.99", "GET", gw.URL+"/api/orders"); resp.StatusCode != 200 {
			t.Errorf("got %d", resp.StatusCode)
		}
	})

	t.Run("bans survive a reload", func(t *testing.T) {
		before := srv.Current()
		if err := srv.Reload(context.Background(), "test"); err != nil {
			t.Fatal(err)
		}
		if srv.Current() == before {
			t.Fatal("reload did not swap the runtime")
		}
		if resp := getAs(t, "203.0.113.1", "GET", gw.URL+"/api/orders"); resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("ban lost on reload: got %d", resp.StatusCode)
		}
	})

	t.Run("unban lifts the ban", func(t *testing.T) {
		ok, err := srv.Current().ScanGuard.Unban(context.Background(), "203.0.113.2")
		if !ok || err != nil {
			t.Fatalf("unban = %v, %v", ok, err)
		}
		if resp := getAs(t, "203.0.113.2", "GET", gw.URL+"/api/orders"); resp.StatusCode != 200 {
			t.Errorf("still blocked after unban: got %d", resp.StatusCode)
		}
	})
}
