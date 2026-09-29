package siding

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// validConfig is the smallest configuration New accepts.
func validConfig() *Config {
	cfg := CreateConfig()
	cfg.Service = "cats"
	cfg.Registry.File = "registry.json"
	return cfg
}

// Every rejection names the key, and the value where there is one: Traefik
// reports the error as it stands, and it is all an operator gets.
func TestNewRejectsBadConfig(t *testing.T) {
	inline := func(tenancies, accounts interface{}) func(*Config) {
		return func(c *Config) {
			c.Registry = &RegistryConfig{Tenancies: tenancies, Accounts: accounts}
		}
	}
	services := func(name, url string) map[string]interface{} {
		return map[string]interface{}{"services": map[string]interface{}{name: url}}
	}
	for want, mutate := range map[string]func(*Config){
		"t: service is required":                     func(c *Config) { c.Service = "" },
		`t: service: invalid service name "Cats"`:    func(c *Config) { c.Service = "Cats" },
		"t: baggageHeader must not be empty":         func(c *Config) { c.BaggageHeader = "" },
		`t: tenancyKey "a b": not a valid name`:      func(c *Config) { c.TenancyKey = "a b" },
		`t: overridesKey "a,b": not a valid name`:    func(c *Config) { c.OverridesKey = "a,b" },
		`t: userHeader "X User": not a valid name`:   func(c *Config) { c.UserHeader = "X User" },
		`t: debugHeader "X:Debug": not a valid name`: func(c *Config) { c.DebugHeader = "X:Debug" },
		`t: tenancyKey and overridesKey are both "k": they must differ`: func(c *Config) {
			c.TenancyKey, c.OverridesKey = "k", "K"
		},
		`t: baggageHeader and userHeader are both "Baggage"`: func(c *Config) {
			c.BaggageHeader, c.UserHeader = "Baggage", "baggage"
		},
		`t: userHeader and debugHeader are both "X-User-Id"`: func(c *Config) {
			c.UserHeader, c.DebugHeader = "X-User-Id", "x-user-id"
		},
		`t: baggageHeader and debugHeader are both "baggage"`: func(c *Config) { c.DebugHeader = "Baggage" },
		`t: debugHeader "Content-Length": that header has a meaning of its own`: func(c *Config) {
			c.DebugHeader = "Content-Length"
		},
		`t: debugHeader "set-cookie": that header has a meaning of its own`: func(c *Config) { c.DebugHeader = "set-cookie" },
		`t: baggageHeader "Host": that header has a meaning of its own`:     func(c *Config) { c.BaggageHeader = "Host" },
		`t: userHeader "Authorization": that header has a meaning of its own`: func(c *Config) {
			c.UserHeader = "Authorization"
		},
		"t: tenancyPrefix must not be empty": func(c *Config) { c.TenancyPrefix = "" },

		"t: allowHeaderOverrides requires allowedHosts": func(c *Config) { c.AllowHeaderOverrides = true },
		`t: allowedHosts[1] "*.example.svc": wildcards are not supported; use a host ("cats.siding-sut.svc:8080") or a leading dot for every host under a domain (".siding-sut.svc:8080")`: func(c *Config) {
			c.AllowedHosts = []string{"sut", "*.example.svc"}
		},
		`t: allowedHosts[0] "http://sut": not a host`:                     func(c *Config) { c.AllowedHosts = []string{"http://sut"} },
		`t: allowedHosts[0] "sut:0": invalid port "0"`:                    func(c *Config) { c.AllowedHosts = []string{"sut:0"} },
		`t: allowedHosts[0] "sut:http": invalid port "http"`:              func(c *Config) { c.AllowedHosts = []string{"sut:http"} },
		`t: allowedHosts[0] "sut:": invalid port ""`:                      func(c *Config) { c.AllowedHosts = []string{"sut:"} },
		`t: allowedHosts[0] "[::1]:65536": invalid port "65536"`:          func(c *Config) { c.AllowedHosts = []string{"[::1]:65536"} },
		`t: allowedHosts[0] "::1": IPv6 literals are written in brackets`: func(c *Config) { c.AllowedHosts = []string{"::1"} },
		`t: allowedHosts[0] "[abc]": invalid IPv6 literal "abc"`:          func(c *Config) { c.AllowedHosts = []string{"[abc]"} },
		`t: allowedHosts[0] "[10.0.0.5]": invalid IPv6 literal "10.0.0.5"`: func(c *Config) {
			c.AllowedHosts = []string{"[10.0.0.5]"}
		},
		`t: allowedHosts[0] "": empty entry`:             func(c *Config) { c.AllowedHosts = []string{""} },
		`t: allowedHosts[0] "a..b": invalid host "a..b"`: func(c *Config) { c.AllowedHosts = []string{"a..b"} },
		`t: allowedHosts[0] ".[::1]": an IP address has no hosts under it`: func(c *Config) {
			c.AllowedHosts = []string{".[::1]"}
		},
		`t: allowedHosts[0] ".0.5:80": an IP address has no hosts under it`: func(c *Config) {
			c.AllowedHosts = []string{".0.5:80"}
		},
		// Clients supply targets: every entry has to say which port.
		`t: allowedHosts[1] ".siding-sut.svc": needs a port when allowHeaderOverrides is on, or a client could reach every port on it: write it as ".siding-sut.svc:8080", with the port the service listens on`: func(c *Config) {
			c.AllowHeaderOverrides = true
			c.AllowedHosts = []string{"sut:80", ".siding-sut.svc"}
		},

		`t: trustedSources[1] "10.42.0.0/33": not a CIDR; use a CIDR ("10.42.0.0/16") or an address ("192.168.1.10")`: func(c *Config) {
			c.TrustedSources = []string{"10.42.0.0/16", "10.42.0.0/33"}
		},
		`t: trustedSources[0] "pods": not an address`: func(c *Config) { c.TrustedSources = []string{"pods"} },
		`t: trustedSources[0] "": empty entry`:        func(c *Config) { c.TrustedSources = []string{""} },
		`t: trustedSources[0] "::ffff:10.42.0.0/112": IPv4-mapped IPv6; write the IPv4 form (10.42.0.0)`: func(c *Config) {
			c.TrustedSources = []string{"::ffff:10.42.0.0/112"}
		},

		`t: registry.refreshInterval "often": not a duration`:      func(c *Config) { c.Registry.RefreshInterval = "often" },
		`t: registry.refreshInterval "500ms": must be at least 1s`: func(c *Config) { c.Registry.RefreshInterval = "500ms" },
		`t: registry.refreshInterval "0": must be at least 1s`:     func(c *Config) { c.Registry.RefreshInterval = "0" },
		`t: registry.refreshInterval "-1s": must be at least 1s`:   func(c *Config) { c.Registry.RefreshInterval = "-1s" },
		"t: registry: set one of inline tenancies/accounts, file or url, not file and url": func(c *Config) {
			c.Registry.URL = "http://registry.svc"
		},
		"t: registry: set one of inline tenancies/accounts, file or url, not inline tenancies/accounts and file": func(c *Config) {
			c.Registry.Tenancies = map[string]interface{}{"test/x": services("cats", "http://sut")}
		},
		`t: registry.url "registry.svc/registry.json": missing scheme`: func(c *Config) {
			c.Registry = &RegistryConfig{URL: "registry.svc/registry.json"}
		},
		`t: registry.url "file:///etc/registry.json": scheme "file" is not http or https`: func(c *Config) {
			c.Registry = &RegistryConfig{URL: "file:///etc/registry.json"}
		},

		`t: registry (inline): tenancy "test/x": invalid URL for service "cats": missing scheme`: inline(
			map[string]interface{}{"test/x": services("cats", "sut:8080")}, nil),
		`t: registry (inline): not a registry document: json: unknown field "service"`: inline(
			map[string]interface{}{"test/x": map[string]interface{}{"service": map[string]interface{}{"cats": "http://sut"}}}, nil),
		`t: registry (inline): not a registry document: json: cannot unmarshal string`: inline("test/x", nil),
		`t: registry (inline): tenancy "test/x" has no services`: inline(
			map[string]interface{}{"test/x": map[string]interface{}{"expiresAt": "2030-01-01T00:00:00Z"}}, nil),
		`t: registry (inline): account "tester-1" is bound to tenancy "test/y", which the registry does not define`: inline(
			map[string]interface{}{"test/x": services("cats", "http://sut")},
			map[string]interface{}{"tester-1": "test/y"}),

		`t: registry (inline): tenancies outside tenancyPrefix "test/" are never routed: "prod/x"`: inline(
			map[string]interface{}{"prod/x": services("cats", "http://sut")}, nil),
		`t: registry (inline): accounts bound to a tenancy outside tenancyPrefix "test/" are never routed: "tester-1"`: inline(
			map[string]interface{}{"test/x": services("cats", "http://sut")},
			map[string]interface{}{"tester-1": "prod"}),

		// A configuration that can do nothing is a mistake too: most likely
		// a misspelt key, which Traefik drops before the plugin sees it.
		"t: nothing can route":                    func(c *Config) { c.Registry = nil },
		"t: nothing can route: set registry.file": func(c *Config) { c.Registry = CreateConfig().Registry },
		`t: userHeader "X-User-Id" needs a registry`: func(c *Config) {
			c.Registry = nil
			c.UserHeader = "X-User-Id"
			c.AllowHeaderOverrides = true
			c.AllowedHosts = []string{"sut:80"}
		},
	} {
		cfg := validConfig()
		mutate(cfg)
		_, err := New(context.Background(), http.NotFoundHandler(), cfg, "t")
		if err == nil {
			t.Errorf("accepted; want %q", want)
		} else if !strings.HasPrefix(err.Error(), want) {
			t.Errorf("got  %q\nwant %q", err, want)
		}
	}
}

func TestNewAcceptsConfig(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"defaults and a file": func(c *Config) {},
		// Only header overrides can route.
		"no registry, header overrides": func(c *Config) {
			c.Registry = nil
			c.AllowHeaderOverrides = true
			c.AllowedHosts = []string{".Siding-SUT.svc:8080", " sut:80 ", "[::1]:9000", "10.0.0.5:443"}
		},
		// The registry is the operator's own: its targets may be bounded by
		// host alone.
		"allowedHosts without ports": func(c *Config) {
			c.AllowedHosts = []string{".Example.SVC", " sut ", "[::1]", "10.0.0.5", "sut:8080"}
		},
		"url": func(c *Config) { c.Registry = &RegistryConfig{URL: "http://127.0.0.1:1/registry.json"} },
		// As Traefik's file provider delivers an empty `accounts: {}`.
		"inline, empty accounts": func(c *Config) {
			c.Registry = &RegistryConfig{
				Tenancies: map[string]interface{}{"test/v1.2/Env": map[string]interface{}{
					"services": map[string]interface{}{"cats": "http://sut"},
				}},
				Accounts: "",
			}
		},
		"trusted sources": func(c *Config) {
			c.TrustedSources = []string{"10.42.0.0/16", " 192.168.1.10", "fd00::/8", "::1"}
		},
	} {
		cfg := validConfig()
		mutate(cfg)
		if _, err := New(context.Background(), http.NotFoundHandler(), cfg, "t"); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestParseRefreshInterval(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"":      10 * time.Second,
		"10s":   10 * time.Second,
		"1m30s": 90 * time.Second,
		"2":     2 * time.Second, // a bare number is seconds, as in Traefik
		" 15 ":  15 * time.Second,
		"1s":    time.Second,
	} {
		if got, err := parseRefreshInterval(raw); err != nil || got != want {
			t.Errorf("%q: got %v, %v; want %v", raw, got, err, want)
		}
	}
	if got := CreateConfig().Registry.RefreshInterval; got != "10s" {
		t.Errorf("default refreshInterval %q", got)
	}
}

func TestParseAllowedHostsNormalises(t *testing.T) {
	got, err := parseAllowedHosts([]string{".Example.SVC", " sut:080 ", "[0:0::1]"}, false)
	if err != nil || len(got.entries) != 3 {
		t.Fatalf("got %+v, %v", got, err)
	}
	for i, want := range []allowedHost{
		{host: "example.svc", suffix: true},
		{host: "sut", port: 80},
		{host: "[::1]"},
	} {
		if got.entries[i] != want {
			t.Errorf("entry %d: got %+v, want %+v", i, got.entries[i], want)
		}
	}
	// No entries is no allowlist, not one that admits nothing.
	if got, err := parseAllowedHosts([]string{}, true); got != nil || err != nil {
		t.Fatalf("got %+v, %v", got, err)
	}
}
