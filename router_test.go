package siding

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const debugHeader = "X-Siding"

// backend records what it received and answers with its own name.
type backend struct {
	name string
	srv  *httptest.Server
	last *http.Request
}

func newBackend(t *testing.T, name string) *backend {
	t.Helper()
	b := &backend{name: name}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.last = r
		_, _ = io.WriteString(w, name+" "+r.URL.RequestURI())
	}))
	// A closure, not the method value b.srv.Close: Yaegi v0.16.1 mistypes
	// method values on stdlib types.
	t.Cleanup(func() { b.srv.Close() })
	return b
}

type fixture struct {
	prod, sut, other *backend
	handler          http.Handler
}

// newFixture builds the middleware in front of prod, with a registry
// mapping test/env-1 → sut for "cats-openapi" and "dogs" → other.
func newFixture(t *testing.T, configure func(*Config)) *fixture {
	t.Helper()
	if configure == nil {
		return newFixtureWith(t, nil)
	}
	return newFixtureWith(t, func(c *Config, _ *fixture) { configure(c) })
}

// endpointOf is a backend's "host:port", as an allowedHosts entry.
func endpointOf(b *backend) string {
	return strings.TrimPrefix(b.srv.URL, "http://")
}

// allowOverrides turns header overrides on for the fixture's sut and other
// backends, and nothing else.
func allowOverrides(c *Config, f *fixture) {
	c.AllowHeaderOverrides = true
	c.AllowedHosts = []string{endpointOf(f.sut), endpointOf(f.other)}
}

// newFixtureWith is newFixture for a configuration that depends on the
// backends, whose ports are only known once they listen.
func newFixtureWith(t *testing.T, configure func(*Config, *fixture)) *fixture {
	t.Helper()
	f := &fixture{
		prod:  newBackend(t, "prod"),
		sut:   newBackend(t, "sut"),
		other: newBackend(t, "other"),
	}
	registry := `{
	  "tenancies": {"test/env-1": {"services": {
	    "cats-openapi": "` + f.sut.srv.URL + `/base",
	    "dogs": "` + f.other.srv.URL + `"}}},
	  "accounts": {"tester-1": "test/env-1", "prod-user": "prod"}
	}`
	cfg := CreateConfig()
	cfg.Service = "cats-openapi"
	cfg.UserHeader = "X-User-Id"
	cfg.DebugHeader = debugHeader
	// Each fixture's temp file is a distinct source, so a distinct registry.
	cfg.Registry = &RegistryConfig{File: writeRegistry(t, registry), RefreshInterval: "1h"}
	if configure != nil {
		configure(cfg, f)
	}
	prodProxy := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveProxy(w, r, f.prod.srv.URL, nil)
	})
	h, err := New(context.Background(), prodProxy, cfg, "test")
	if err != nil {
		t.Fatal(err)
	}
	f.handler = h
	return f
}

func (f *fixture) do(t *testing.T, headers map[string]string) (string, *httptest.ResponseRecorder) {
	t.Helper()
	return f.send(t, "", "/cats?id=1", headers)
}

// send is do for a given peer address and request path; an empty peer is
// httptest's default, 192.0.2.1.
func (f *fixture) send(t *testing.T, peer, path string, headers map[string]string) (string, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "http://cats.example.com"+path, nil)
	if peer != "" {
		req.RemoteAddr = peer
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec.Body.String(), rec
}

func TestNoBaggageGoesToProd(t *testing.T) {
	f := newFixture(t, nil)
	body, rec := f.do(t, nil)
	if body != "prod /cats?id=1" {
		t.Fatalf("got %q", body)
	}
	if h := rec.Header().Get(debugHeader); h != "" {
		t.Fatalf("debug header on plain traffic: %q", h)
	}
	if f.prod.last.Header.Get("Baggage") != "" {
		t.Fatal("baggage injected into plain traffic")
	}
}

func TestRegistryTenancyRoutesToSUT(t *testing.T) {
	f := newFixture(t, nil)
	body, rec := f.do(t, map[string]string{"Baggage": "request-tenancy=test/env-1;ttl=60,other=x"})
	if body != "sut /base/cats?id=1" {
		t.Fatalf("got %q", body)
	}
	if got := rec.Header().Get(debugHeader); got != "routed="+f.sut.srv.URL+"/base" {
		t.Fatalf("debug header %q", got)
	}
	if host := f.sut.last.Host; host != "cats.example.com" {
		t.Fatalf("SUT saw Host %q", host)
	}
	// Edge injection: the SUT's own downstream calls carry the overrides.
	bag, _ := parseBaggage(f.sut.last.Header.Values("Baggage"))
	raw, ok := bag.get("routing-overrides")
	if !ok {
		t.Fatalf("routing-overrides not injected: %q", f.sut.last.Header.Get("Baggage"))
	}
	o, err := decodeOverrides(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if url, _ := o.lookup("dogs"); url != f.other.srv.URL {
		t.Fatalf("injected overrides %+v", o)
	}
	if v, _ := bag.get("other"); v != "x" {
		t.Fatal("unrelated baggage member lost")
	}
	// A tenancy member that already says the right thing is not rewritten.
	if got := f.sut.last.Header.Get("Baggage"); !strings.HasPrefix(got, "request-tenancy=test/env-1;ttl=60,") {
		t.Fatalf("tenancy member rewritten: %q", got)
	}
}

func TestTenancyForAnotherServiceGoesToProd(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.Service = "birds" })
	body, rec := f.do(t, map[string]string{"Baggage": "request-tenancy=test/env-1"})
	if !strings.HasPrefix(body, "prod ") {
		t.Fatalf("got %q", body)
	}
	if got := rec.Header().Get(debugHeader); got != "fallback=no override for service birds" {
		t.Fatalf("debug header %q", got)
	}
	// Still injected, so a later hop to cats-openapi routes to the SUT.
	if bag, _ := parseBaggage(f.prod.last.Header.Values("Baggage")); !hasKey(bag, "routing-overrides") {
		t.Fatal("overrides not injected on fallback")
	}
}

func TestUnregisteredOrUnprefixedTenancyGoesToProd(t *testing.T) {
	f := newFixture(t, nil)
	for baggage, reason := range map[string]string{
		"request-tenancy=test/unknown": `fallback=tenancy "test/unknown" is not registered`,
		"request-tenancy=production":   `fallback=tenancy "production" is not under "test/"`,
	} {
		body, rec := f.do(t, map[string]string{"Baggage": baggage})
		if !strings.HasPrefix(body, "prod ") {
			t.Errorf("%s: got %q", baggage, body)
		}
		if got := rec.Header().Get(debugHeader); got != reason {
			t.Errorf("%s: debug header %q, want %q", baggage, got, reason)
		}
		// The tenancy is other systems' data too: it is passed on as it came.
		if got := f.prod.last.Header.Get("Baggage"); got != baggage {
			t.Errorf("%s: forwarded as %q", baggage, got)
		}
	}
}

func TestRegistryNotLoadedSaysSo(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.Registry.File = c.Registry.File + ".missing" })
	body, rec := f.do(t, map[string]string{"Baggage": "request-tenancy=test/env-1"})
	if !strings.HasPrefix(body, "prod ") {
		t.Fatalf("got %q", body)
	}
	if got := rec.Header().Get(debugHeader); got != "fallback=registry not loaded" {
		t.Fatalf("debug header %q", got)
	}
}

func TestAccountBindingRoutesToSUT(t *testing.T) {
	f := newFixture(t, nil)
	if body, _ := f.do(t, map[string]string{"X-User-Id": "tester-1"}); !strings.HasPrefix(body, "sut ") {
		t.Fatalf("got %q", body)
	}
	if v, _ := mustBaggage(t, f.sut.last).get("request-tenancy"); v != "test/env-1" {
		t.Fatalf("tenancy not injected: %q", v)
	}
	// An account bound outside the prefix, and an unknown account, go to prod.
	for _, user := range []string{"prod-user", "stranger"} {
		if body, _ := f.do(t, map[string]string{"X-User-Id": user}); !strings.HasPrefix(body, "prod ") {
			t.Errorf("%s: got %q", user, body)
		}
	}
	// Baggage tenancy takes precedence over the account.
	body, _ := f.do(t, map[string]string{"X-User-Id": "tester-1", "Baggage": "request-tenancy=test/unknown"})
	if !strings.HasPrefix(body, "prod ") {
		t.Fatalf("baggage tenancy did not win: %q", body)
	}
	// An empty tenancy member is no tenancy, so the account still applies.
	body, _ = f.do(t, map[string]string{"X-User-Id": "tester-1", "Baggage": "request-tenancy="})
	if !strings.HasPrefix(body, "sut ") {
		t.Fatalf("empty tenancy member suppressed the account: %q", body)
	}
}

func TestHeaderOverridesDisabledAreStripped(t *testing.T) {
	f := newFixture(t, nil)
	evil := (&Overrides{Services: []Service{{Name: "cats-openapi", URL: f.other.srv.URL}}}).encode()
	body, rec := f.do(t, map[string]string{"Baggage": "routing-overrides=" + evil + ",keep=1"})
	if !strings.HasPrefix(body, "prod ") {
		t.Fatalf("got %q", body)
	}
	if got := rec.Header().Get(debugHeader); got != "fallback=header overrides are not allowed" {
		t.Fatalf("debug header %q", got)
	}
	if got := f.prod.last.Header.Get("Baggage"); got != "keep=1" {
		t.Fatalf("forged overrides forwarded: %q", got)
	}

	// Stripping the only member leaves no header, rather than an empty one.
	f.do(t, map[string]string{"Baggage": "routing-overrides=" + evil})
	if got := f.prod.last.Header.Values("Baggage"); len(got) != 0 {
		t.Fatalf("baggage header left behind: %q", got)
	}
}

// A member whose value doesn't percent-decode, or a second member with the
// same key, must not reach a later hop that reads baggage more leniently.
func TestMalformedAndDuplicateRoutingMembersAreRemoved(t *testing.T) {
	f := newFixture(t, nil)
	evil := (&Overrides{Services: []Service{{Name: "cats-openapi", URL: f.other.srv.URL}}}).encode()
	for baggage, want := range map[string]string{
		"routing-overrides=" + evil + "%zz,keep=1":                           "keep=1",
		"request-tenancy=production,request-tenancy=test/env-1,keep=1":       "request-tenancy=production,keep=1",
		"request-tenancy=%zz,keep=1":                                         "keep=1",
		"keep=1,request-tenancy=production,routing-overrides=%zz,broken=%zz": "keep=1,request-tenancy=production,broken=%zz",
	} {
		body, _ := f.do(t, map[string]string{"Baggage": baggage})
		if !strings.HasPrefix(body, "prod ") {
			t.Errorf("%s: got %q", baggage, body)
		}
		if got := f.prod.last.Header.Get("Baggage"); got != want {
			t.Errorf("%s: forwarded as %q, want %q", baggage, got, want)
		}
	}
}

func TestHeaderOverridesRejectedFallBackToRegistry(t *testing.T) {
	f := newFixture(t, nil)
	evil := (&Overrides{Services: []Service{{Name: "cats-openapi", URL: f.other.srv.URL}}}).encode()
	body, _ := f.do(t, map[string]string{"Baggage": "request-tenancy=test/env-1,routing-overrides=" + evil})
	if !strings.HasPrefix(body, "sut ") {
		t.Fatalf("got %q", body)
	}
}

// Which reason a request that stayed with production is given, when more
// than one thing was wrong with it.
func TestFallbackReasons(t *testing.T) {
	evil := (&Overrides{Services: []Service{{Name: "cats-openapi", URL: "http://evil.example"}}}).encode()
	forged := "routing-overrides=" + evil
	tenancy := "request-tenancy=test/env-1"

	for name, c := range map[string]struct {
		configure func(*Config, *fixture)
		baggage   string
		want      string
	}{
		"forged overrides, nothing else": {nil, forged, "header overrides are not allowed"},
		"forged overrides and an unknown tenancy: the header's": {
			nil, forged + ",request-tenancy=test/nope", "header overrides are not allowed",
		},
		"forged overrides, and the registry has nothing for this service": {
			func(c *Config, _ *fixture) { c.Service = "birds" }, forged + "," + tenancy, "no override for service birds",
		},
		// What the registry has to say replaces what was said of the header.
		"forged overrides, and the registry's target is not allowed": {
			func(c *Config, _ *fixture) { c.AllowedHosts = []string{"elsewhere.example"} }, forged + "," + tenancy,
			`tenancy "test/env-1": "127.0.0.1:`,
		},
		"a tenancy outside the prefix comes before anything else": {
			nil, forged + ",request-tenancy=production", `tenancy "production" is not under "test/"`,
		},
		"no registry": {
			// Two statements: Yaegi v0.16.1 panics on a tuple assignment
			// that has nil in it.
			func(c *Config, f *fixture) {
				c.Registry = nil
				c.UserHeader = ""
				allowOverrides(c, f)
			}, tenancy, "no registry configured",
		},
	} {
		f := newFixtureWith(t, c.configure)
		body, rec := f.do(t, map[string]string{"Baggage": c.baggage})
		if !strings.HasPrefix(body, "prod ") {
			t.Errorf("%s: got %q", name, body)
		}
		if got := rec.Header().Get(debugHeader); !strings.HasPrefix(got, "fallback="+c.want) {
			t.Errorf("%s: debug header %q, want %q", name, got, c.want)
		}
	}
}

func TestHeaderOverridesAllowlisted(t *testing.T) {
	f := newFixtureWith(t, allowOverrides)
	good := (&Overrides{Services: []Service{{Name: "cats-openapi", URL: f.other.srv.URL}}}).encode()
	// Explicit overrides win over the tenancy's registry entry.
	body, _ := f.do(t, map[string]string{"Baggage": "request-tenancy=test/env-1,routing-overrides=" + good})
	if !strings.HasPrefix(body, "other ") {
		t.Fatalf("got %q", body)
	}
	// They need no tenancy at all.
	body, _ = f.do(t, map[string]string{"Baggage": "routing-overrides=" + good})
	if !strings.HasPrefix(body, "other ") {
		t.Fatalf("overrides alone: got %q", body)
	}

	// Neither another host nor another port on an allowed host gets through.
	for _, url := range []string{"http://evil.example", "http://127.0.0.1:1", "http://127.0.0.1"} {
		bad := (&Overrides{Services: []Service{{Name: "cats-openapi", URL: url}}}).encode()
		body, rec := f.do(t, map[string]string{"Baggage": "routing-overrides=" + bad})
		if !strings.HasPrefix(body, "prod ") {
			t.Fatalf("%s: got %q", url, body)
		}
		if got := rec.Header().Get(debugHeader); !strings.Contains(got, "not in allowedHosts") {
			t.Fatalf("%s: debug header %q", url, got)
		}
		if hasKey(mustBaggage(t, f.prod.last), "routing-overrides") {
			t.Fatalf("%s: rejected overrides forwarded", url)
		}
	}
}

// tenancyPrefix is the one rule nothing gets around: valid, allowlisted
// overrides don't route a request that says it belongs to production.
func TestTenancyOutsidePrefixNeverRoutes(t *testing.T) {
	f := newFixtureWith(t, allowOverrides)
	good := (&Overrides{Services: []Service{{Name: "cats-openapi", URL: f.other.srv.URL}}}).encode()
	body, rec := f.do(t, map[string]string{"Baggage": "request-tenancy=production,routing-overrides=" + good})
	if !strings.HasPrefix(body, "prod ") {
		t.Fatalf("got %q", body)
	}
	if got := rec.Header().Get(debugHeader); got != `fallback=tenancy "production" is not under "test/"` {
		t.Fatalf("debug header %q", got)
	}
	if got := f.prod.last.Header.Get("Baggage"); got != "request-tenancy=production" {
		t.Fatalf("forwarded as %q", got)
	}
}

// allowedHosts is applied to each registry target on its own: one target it
// rejects does not cost the tenancy its other services.
func TestAllowedHostsAppliesPerRegistryTarget(t *testing.T) {
	_, warn := captureLogs(t)
	var sut *backend
	configure := func(service string) func(*Config) {
		return func(c *Config) {
			sut = newBackend(t, "sut")
			c.Service = service
			c.AllowedHosts = []string{"127.0.0.1"}
			c.Registry.File = writeRegistry(t, `{"tenancies": {"test/env-1": {"services": {
			  "cats-openapi": "`+sut.srv.URL+`",
			  "dogs": "http://dogs.example:8080"}}}}`)
		}
	}
	headers := map[string]string{"Baggage": "request-tenancy=test/env-1"}

	f := newFixture(t, configure("cats-openapi"))
	if body, _ := f.do(t, headers); !strings.HasPrefix(body, "sut ") {
		t.Fatalf("got %q", body)
	}
	// What is injected holds only what a later hop would accept.
	raw, _ := mustBaggage(t, sut.last).get("routing-overrides")
	o, err := decodeOverrides(raw, allow(t, "127.0.0.1"))
	if err != nil {
		t.Fatalf("injected overrides: %v", err)
	}
	if _, ok := o.lookup("dogs"); ok || len(o.Services) != 1 {
		t.Fatalf("injected overrides %+v", o)
	}
	f.do(t, headers)
	if got := strings.Count(warn.String(), `http://dogs.example:8080 for service "dogs" is ignored`); got != 1 {
		t.Fatalf("dropped target logged %d times: %q", got, warn.String())
	}

	f = newFixture(t, configure("dogs"))
	body, rec := f.do(t, headers)
	if !strings.HasPrefix(body, "prod ") {
		t.Fatalf("got %q", body)
	}
	want := `fallback=tenancy "test/env-1": "dogs.example:8080" for service "dogs" is not in allowedHosts`
	if got := rec.Header().Get(debugHeader); got != want {
		t.Fatalf("debug header %q", got)
	}
}

func TestInlineRegistry(t *testing.T) {
	sut := newBackend(t, "sut")
	f := newFixture(t, func(c *Config) {
		// As Traefik's file provider delivers it: untyped maps of strings.
		c.Registry = &RegistryConfig{
			RefreshInterval: "10s",
			Tenancies: map[string]interface{}{
				"test/env-1": map[string]interface{}{
					"services": map[string]interface{}{"cats-openapi": sut.srv.URL},
				},
				"test/expired": map[string]interface{}{
					"services":  map[string]interface{}{"cats-openapi": sut.srv.URL},
					"expiresAt": "2020-01-01T00:00:00Z",
				},
			},
			Accounts: map[string]interface{}{"tester-1": "test/env-1"},
		}
	})
	if body, _ := f.do(t, map[string]string{"Baggage": "request-tenancy=test/env-1"}); !strings.HasPrefix(body, "sut ") {
		t.Fatalf("got %q", body)
	}
	if body, _ := f.do(t, map[string]string{"X-User-Id": "tester-1"}); !strings.HasPrefix(body, "sut ") {
		t.Fatalf("account: got %q", body)
	}
	body, rec := f.do(t, map[string]string{"Baggage": "request-tenancy=test/expired"})
	if !strings.HasPrefix(body, "prod ") {
		t.Fatalf("expired: got %q", body)
	}
	if got := rec.Header().Get(debugHeader); got != `fallback=tenancy "test/expired" expired at 2020-01-01T00:00:00Z` {
		t.Fatalf("debug header %q", got)
	}
}

func TestTrustedSources(t *testing.T) {
	f := newFixtureWith(t, func(c *Config, f *fixture) {
		c.TrustedSources = []string{"10.42.0.0/16", " 192.168.1.10", "fd00::/8"}
		allowOverrides(c, f)
	})
	tenancy := map[string]string{"Baggage": "request-tenancy=test/env-1,keep=1"}

	for _, peer := range []string{
		"10.42.3.7:40000",
		"192.168.1.10:40000",
		"[::ffff:10.42.3.7]:40000", // IPv4 seen through a dual-stack listener
		"[fd00::1]:40000",
	} {
		body, rec := f.send(t, peer, "/cats", tenancy)
		if !strings.HasPrefix(body, "sut ") {
			t.Errorf("%s: got %q", peer, body)
		}
		if rec.Header().Get(debugHeader) == "" {
			t.Errorf("%s: no debug header for a trusted peer", peer)
		}
	}

	overrides := (&Overrides{Services: []Service{{Name: "cats-openapi", URL: f.other.srv.URL}}}).encode()
	for _, peer := range []string{
		"203.0.113.9:40000",
		"192.168.1.11:40000",
		"10.43.0.1:40000",
		"not an address",
	} {
		for _, baggage := range []string{
			"request-tenancy=test/env-1,keep=1",
			"Request-Tenancy=test/env-1,keep=1",
			"keep=1,routing-overrides=" + overrides,
			"request-tenancy=test/env-1%zz,keep=1",
		} {
			f.prod.last = nil
			body, rec := f.send(t, peer, "/cats", map[string]string{"Baggage": baggage})
			if !strings.HasPrefix(body, "prod ") {
				t.Errorf("%s %s: got %q", peer, baggage, body)
				continue
			}
			if got := f.prod.last.Header.Get("Baggage"); got != "keep=1" {
				t.Errorf("%s %s: forwarded as %q", peer, baggage, got)
			}
			// The debug header names internal hosts.
			if got := rec.Header().Values(debugHeader); len(got) != 0 {
				t.Errorf("%s %s: debug header %q", peer, baggage, got)
			}
		}
	}

	// Padding the header past the limit doesn't carry routing keys through.
	huge := "request-tenancy=test/env-1," + strings.Repeat("x", maxBaggageBytes)
	body, _ := f.send(t, "203.0.113.9:40000", "/cats", map[string]string{"Baggage": huge})
	if !strings.HasPrefix(body, "prod ") {
		t.Fatalf("oversized: got %q", body)
	}
	if got := f.prod.last.Header.Values("Baggage"); len(got) != 0 {
		t.Fatalf("oversized baggage from an untrusted peer forwarded: %d bytes", len(got[0]))
	}

	// The account binding is the way in for an untrusted peer, and other
	// baggage is left alone.
	body, _ = f.send(t, "203.0.113.9:40000", "/cats", map[string]string{"X-User-Id": "tester-1", "Baggage": "keep=1;p"})
	if !strings.HasPrefix(body, "sut ") {
		t.Fatalf("account from an untrusted peer: got %q", body)
	}
	if got := f.sut.last.Header.Get("Baggage"); !strings.HasPrefix(got, "keep=1;p,request-tenancy=test/env-1,routing-overrides=") {
		t.Fatalf("forwarded as %q", got)
	}
	body, _ = f.send(t, "203.0.113.9:40000", "/cats", map[string]string{"Baggage": "keep=1;p"})
	if got := f.prod.last.Header.Get("Baggage"); !strings.HasPrefix(body, "prod ") || got != "keep=1;p" {
		t.Fatalf("plain traffic from an untrusted peer: %q, forwarded as %q", body, got)
	}
}

// What is forwarded has to be something the next hop will read: a list over
// the W3C limits is refused whole, and the request's later hops would go to
// production.
func TestInjectionStaysWithinTheLimits(t *testing.T) {
	info, _ := captureLogs(t)
	f := newFixture(t, nil)
	account := map[string]string{"X-User-Id": "tester-1"}
	forwarded := func(baggage string) *baggage {
		t.Helper()
		account["Baggage"] = baggage
		if body, _ := f.do(t, account); !strings.HasPrefix(body, "sut ") {
			t.Fatalf("%.30s…: not routed: %q", baggage, body)
		}
		got := f.sut.last.Header.Get("Baggage")
		bag, err := parseBaggage([]string{got})
		if err != nil {
			t.Fatalf("%.30s…: the next hop refuses what was forwarded (%d bytes): %v", baggage, len(got), err)
		}
		return bag
	}
	members := func(n int) string { return strings.TrimSuffix(strings.Repeat("k=v,", n), ",") }
	filler := func(n int) string { return "other=" + strings.Repeat("x", n-len("other=")) }

	// Room for both.
	if bag := forwarded(members(178)); !hasKey(bag, "request-tenancy") || !hasKey(bag, "routing-overrides") {
		t.Errorf("178 members: got %d, without both routing members", len(bag.members))
	}
	// Room for one: the tenancy, which a later hop can look up.
	for name, baggage := range map[string]string{"179 members": members(179), "8150 bytes": filler(8150)} {
		if bag := forwarded(baggage); !hasKey(bag, "request-tenancy") || hasKey(bag, "routing-overrides") {
			t.Errorf("%s: want the tenancy alone", name)
		}
	}
	// Room for neither: forwarded as it came.
	for name, baggage := range map[string]string{"180 members": members(180), "8190 bytes": filler(8190)} {
		if bag := forwarded(baggage); hasKey(bag, "request-tenancy") || hasKey(bag, "routing-overrides") {
			t.Errorf("%s: injected past the limit", name)
		}
		if got := f.sut.last.Header.Get("Baggage"); got != baggage {
			t.Errorf("%s: rewritten", name)
		}
	}
	if got := info.String(); !strings.Contains(got, "injected without its overrides") || !strings.Contains(got, "not injected") {
		t.Fatalf("not logged: %q", got)
	}
}

// Overrides that arrived with the request are a peer's: they are not dropped
// to make room for the tenancy.
func TestInjectionKeepsOverridesThatArrived(t *testing.T) {
	f := newFixtureWith(t, allowOverrides)
	good := (&Overrides{Services: []Service{{Name: "cats-openapi", URL: f.other.srv.URL}}}).encode()
	baggage := "routing-overrides=" + good + "," + strings.TrimSuffix(strings.Repeat("k=v,", 179), ",")
	body, _ := f.do(t, map[string]string{"X-User-Id": "tester-1", "Baggage": baggage})
	if !strings.HasPrefix(body, "other ") {
		t.Fatalf("got %q", body)
	}
	bag := mustBaggage(t, f.other.last)
	if got, _ := bag.get("routing-overrides"); got != good || hasKey(bag, "request-tenancy") {
		t.Fatalf("forwarded as %.80q…", f.other.last.Header.Get("Baggage"))
	}
}

func TestSUTDownIs502(t *testing.T) {
	f := newFixture(t, nil)
	f.sut.srv.Close()
	body, rec := f.do(t, map[string]string{"Baggage": "request-tenancy=test/env-1"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d, body %q", rec.Code, body)
	}
	if f.prod.last != nil {
		t.Fatal("fell back to prod after a SUT failure")
	}
}

// A SUT that is down is reported, not once per request: any client that
// names its tenancy would otherwise fill the log at ERROR.
func TestSUTDownIsLoggedOncePerWindow(t *testing.T) {
	_, warn := captureLogs(t)
	f := newFixture(t, nil)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	f.handler.(*Router).failures.now = func() time.Time { return now }
	f.sut.srv.Close()
	fail := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			if _, rec := f.do(t, map[string]string{"Baggage": "request-tenancy=test/env-1"}); rec.Code != http.StatusBadGateway {
				t.Fatalf("status %d", rec.Code)
			}
		}
	}

	fail(25)
	if got := strings.Count(warn.String(), "proxy to "+f.sut.srv.URL); got != 1 {
		t.Fatalf("25 failures logged %d times: %q", got, warn.String())
	}
	now = now.Add(failureWindow)
	fail(3)
	got := warn.String()
	if strings.Count(got, "proxy to ") != 2 || !strings.Contains(got, "(and 24 more failures since the last one reported)") {
		t.Fatalf("after the window: %q", got)
	}
}

func TestFailureLogBoundsTheTargetsItTellsApart(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newFailureLog()
	l.now = func() time.Time { return now }
	logged := 0
	for round := 0; round < 3; round++ {
		for i := 0; i < maxFailureTargets+40; i++ {
			if ok, _ := l.report("http://sut-" + strconv.Itoa(i)); ok {
				logged++
			}
		}
	}
	// Each of the first targets once, and the rest once between them.
	if logged != maxFailureTargets+1 || len(l.targets) != maxFailureTargets+1 {
		t.Fatalf("logged %d lines, remembers %d targets", logged, len(l.targets))
	}

	// Targets that stopped failing make room for others.
	now = now.Add(failureWindow)
	for target, f := range l.targets {
		if target != "http://sut-0" {
			f.unreported = 0
		}
	}
	if ok, _ := l.report("http://sut-new"); !ok {
		t.Fatal("a new target was not reported")
	}
	if _, kept := l.targets["http://sut-0"]; !kept || len(l.targets) != 2 {
		t.Fatalf("remembers %d targets", len(l.targets))
	}
}

func TestInjectBaggageDisabled(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.InjectBaggage = false })
	body, _ := f.do(t, map[string]string{"X-User-Id": "tester-1"})
	if !strings.HasPrefix(body, "sut ") {
		t.Fatalf("got %q", body)
	}
	if got := f.sut.last.Header.Get("Baggage"); got != "" {
		t.Fatalf("baggage injected: %q", got)
	}
}

func TestOversizedBaggagePassesThroughUntouched(t *testing.T) {
	f := newFixture(t, nil)
	huge := "other=1," + strings.Repeat("x", maxBaggageBytes)
	body, rec := f.do(t, map[string]string{"X-User-Id": "tester-1", "Baggage": huge})
	if !strings.HasPrefix(body, "prod ") {
		t.Fatalf("got %q", body)
	}
	if got := rec.Header().Get(debugHeader); got != "fallback="+errBaggageTooLarge.Error() {
		t.Fatalf("debug header %q", got)
	}
	if got := f.prod.last.Header.Get("Baggage"); got != huge {
		t.Fatalf("oversized baggage rewritten: %d bytes", len(got))
	}
}

// Past the limits nothing is parsed, so nothing is stripped either: a header
// that names a routing key is dropped whole, or padding it would carry a
// forged member to the next hop.
func TestOversizedBaggageNamingARoutingKeyIsDropped(t *testing.T) {
	f := newFixture(t, nil)
	evil := (&Overrides{Services: []Service{{Name: "cats-openapi", URL: f.other.srv.URL}}}).encode()
	pad := "pad=" + strings.Repeat("x", maxBaggageBytes)
	for _, baggage := range []string{
		"routing-overrides=" + evil + "," + pad,
		"Routing-Overrides=" + evil + "," + pad,
		"request-tenancy=test/env-1," + pad,
		"request-tenancy=test/env-1," + strings.Repeat("k=v,", maxBaggageMembers),
	} {
		body, rec := f.do(t, map[string]string{"Baggage": baggage})
		if !strings.HasPrefix(body, "prod ") {
			t.Errorf("%.40s…: got %q", baggage, body)
		}
		if got := rec.Header().Get(debugHeader); got != "fallback="+errBaggageTooLarge.Error() {
			t.Errorf("%.40s…: debug header %q", baggage, got)
		}
		if got := f.prod.last.Header.Values("Baggage"); len(got) != 0 {
			t.Errorf("%.40s…: %d bytes forwarded", baggage, len(got[0]))
		}
	}
	// The account binding doesn't depend on the baggage.
	body, _ := f.do(t, map[string]string{"X-User-Id": "tester-1", "Baggage": "request-tenancy=production," + pad})
	if !strings.HasPrefix(body, "sut ") {
		t.Fatalf("account with dropped baggage: got %q", body)
	}
}

// A routing key is this middleware's in any case: a member that differs from
// it only in case could be read as it by a later hop.
func TestCaseVariantsOfRoutingKeysAreRemoved(t *testing.T) {
	evil := (&Overrides{Services: []Service{{Name: "cats-openapi", URL: "http://127.0.0.1:1"}}}).encode()
	f := newFixture(t, nil)
	for baggage, want := range map[string]string{
		"Routing-Overrides=" + evil + ",keep=1":                        "keep=1",
		"keep=1,ROUTING-OVERRIDES=" + evil:                             "keep=1",
		"request-tenancy=production,Routing-Overrides=" + evil:         "request-tenancy=production",
		"request-tenancy=production,Request-Tenancy=test/env-1,keep=1": "request-tenancy=production,keep=1",
	} {
		body, _ := f.do(t, map[string]string{"Baggage": baggage})
		if !strings.HasPrefix(body, "prod ") {
			t.Errorf("%s: got %q", baggage, body)
		}
		if got := f.prod.last.Header.Get("Baggage"); got != want {
			t.Errorf("%s: forwarded as %q, want %q", baggage, got, want)
		}
	}

	// With overrides allowed, the exact key is the one that counts.
	f = newFixtureWith(t, allowOverrides)
	good := (&Overrides{Services: []Service{{Name: "cats-openapi", URL: f.other.srv.URL}}}).encode()
	body, _ := f.do(t, map[string]string{"Baggage": "routing-overrides=" + good + ",Routing-Overrides=" + evil})
	if !strings.HasPrefix(body, "other ") {
		t.Fatalf("got %q", body)
	}
	if got := f.other.last.Header.Get("Baggage"); strings.Contains(got, "Routing-Overrides") || strings.Contains(got, evil) {
		t.Fatalf("forwarded as %q", got)
	}
}

func TestDebugHeaderIsOffByDefault(t *testing.T) {
	if got := CreateConfig().DebugHeader; got != "" {
		t.Fatalf("default debugHeader %q", got)
	}
	f := newFixture(t, func(c *Config) { c.DebugHeader = "" })
	_, rec := f.do(t, map[string]string{"Baggage": "request-tenancy=test/env-1"})
	if len(rec.Header().Values(debugHeader)) != 0 {
		t.Fatal("debug header set while disabled")
	}
}

// The debug header's reasons quote the request. They stay a bounded, ASCII,
// single line whatever the client sent.
func TestDebugHeaderIsBoundedAndASCII(t *testing.T) {
	f := newFixture(t, nil)
	for name, tenancy := range map[string]string{
		"4000 bytes":    strings.Repeat("A", 4000),
		"a line break":  "prod%0d%0aX-Injected:%201",
		"not ASCII":     "prod/caf%C3%A9",
		"invalid UTF-8": "prod/%ff%fe" + strings.Repeat("%c3", 200),
	} {
		_, rec := f.do(t, map[string]string{"Baggage": "request-tenancy=" + tenancy})
		got := rec.Header().Get(debugHeader)
		if !strings.HasPrefix(got, `fallback=tenancy "`) || len(got) > maxDebugValue {
			t.Errorf("%s: %d bytes: %q", name, len(got), got)
		}
		for i := 0; i < len(got); i++ {
			if got[i] < ' ' || got[i] > '~' {
				t.Errorf("%s: byte %#x at %d: %q", name, got[i], i, got)
				break
			}
		}
	}
}

// An escaped slash is part of a path segment. The SUT has to see the path
// the production backend would.
func TestEscapedPathReachesTheSUTUnchanged(t *testing.T) {
	f := newFixture(t, nil)
	for path, want := range map[string]string{
		"/files/a%2Fb?x=1": "/base/files/a%2Fb?x=1",
		"/files/a%20b":     "/base/files/a%20b",
		"/":                "/base/",
	} {
		body, _ := f.send(t, "", path, map[string]string{"Baggage": "request-tenancy=test/env-1"})
		if body != "sut "+want {
			t.Errorf("%s: got %q, want %q", path, body, "sut "+want)
		}
		if body, _ := f.send(t, "", path, nil); body != "prod "+path {
			t.Errorf("%s: prod got %q", path, body)
		}
	}
}

func TestJoinURLPath(t *testing.T) {
	for _, c := range [][4]string{
		{"", "/a", "/a", ""},
		{"/", "/a", "/a", ""},
		{"", "", "/", ""},
		{"/base", "/", "/base/", ""},
		{"/base", "/a/b", "/base/a/b", ""},
		{"/base/", "/a", "/base/a", ""},
		{"/base", "/a%2Fb", "/base/a/b", "/base/a%2Fb"},
		{"/ba%2Fse/", "/a", "/ba/se/a", "/ba%2Fse/a"},
	} {
		a, err := url.Parse("http://sut" + c[0])
		if err != nil {
			t.Fatal(err)
		}
		b, err := url.Parse("http://prod" + c[1])
		if err != nil {
			t.Fatal(err)
		}
		if path, raw := joinURLPath(a, b); path != c[2] || raw != c[3] {
			t.Errorf("joinURLPath(%q, %q) = %q, %q; want %q, %q", c[0], c[1], path, raw, c[2], c[3])
		}
	}
}

func mustBaggage(t *testing.T, r *http.Request) *baggage {
	t.Helper()
	b, err := parseBaggage(r.Header.Values("Baggage"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func hasKey(b *baggage, key string) bool {
	_, ok := b.get(key)
	return ok
}
