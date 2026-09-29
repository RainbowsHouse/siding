package siding

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func encodePayload(json string) string {
	return base64.StdEncoding.EncodeToString([]byte(json))
}

func TestDecodeOverridesValid(t *testing.T) {
	o, err := decodeOverrides(encodePayload(`{"services":[
		{"name":"cats-openapi","url":"https://test-2.example.internal"},
		{"name":"dogs-api","url":"https://dogs.example.com"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Services) != 2 || o.Services[1].Name != "dogs-api" {
		t.Fatalf("got %+v", o.Services)
	}
	if url, ok := o.lookup("cats-openapi"); !ok || url != "https://test-2.example.internal" {
		t.Fatalf("lookup = %q, %v", url, ok)
	}
}

func TestDecodeOverridesAcceptsEveryBase64Flavour(t *testing.T) {
	json := []byte(`{"services":[{"name":"a","url":"http://a.svc/?x=>>>"}]}`)
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	} {
		if _, err := decodeOverrides(enc.EncodeToString(json), nil); err != nil {
			t.Errorf("%v", err)
		}
	}
}

func TestOverridesEncodeRoundTrip(t *testing.T) {
	in := &Overrides{Services: []Service{{Name: "a", URL: "http://a.svc:8080/x?y=1"}}}
	enc := in.encode()
	for i := 0; i < len(enc); i++ {
		if !isBaggageOctet(enc[i]) || enc[i] == '%' {
			t.Fatalf("encoding %q needs percent-escaping", enc)
		}
	}
	out, err := decodeOverrides(enc, nil)
	if err != nil || out.Services[0] != in.Services[0] {
		t.Fatalf("got %+v, %v", out, err)
	}
}

func TestDecodeOverridesRejects(t *testing.T) {
	many := make([]string, maxServices+1)
	for i := range many {
		many[i] = fmt.Sprintf(`{"name":"svc-%d","url":"https://a.example"}`, i)
	}
	cases := []struct{ payload, want string }{
		{"!!!not base64", "base64"},
		{encodePayload("not json"), "json"},
		{encodePayload(`{"services":[{"name":"evil\r\nx-injected: 1","url":"https://a.example"}]}`), "invalid service name"},
		{encodePayload(`{"services":[{"name":"` + strings.Repeat("a", 33) + `","url":"https://a.example"}]}`), "invalid service name"},
		{encodePayload(`{"services":[{"name":"ok","url":"https://a.example\r\nx-bad: 1"}]}`), "invalid URL"},
		{encodePayload(`{"services":[` + strings.Join(many, ",") + `]}`), "too many services"},
		// One bad entry rejects the whole payload.
		{encodePayload(`{"services":[{"name":"good","url":"https://a.example"},{"name":"BAD","url":"https://b.example"}]}`), "BAD"},
	}
	for _, c := range cases {
		_, err := decodeOverrides(c.payload, nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want %q", c.payload, err, c.want)
		}
	}
	for _, name := range []string{"", "Upper", "under_score", "dot.ted", "sp ace"} {
		if validateServiceName(name) == nil {
			t.Errorf("name %q accepted", name)
		}
	}
}

func TestDecodeOverridesEmptyServices(t *testing.T) {
	o, err := decodeOverrides(encodePayload(`{"services":[]}`), nil)
	if err != nil || len(o.Services) != 0 {
		t.Fatalf("got %+v, %v", o, err)
	}
}

func TestValidateServiceURLAccepts(t *testing.T) {
	for _, u := range []string{
		"http://cats.example.svc",
		"https://cats.example.svc.cluster.local:8443/v1?x=1#frag",
		"HTTP://cats.example.svc",
		"http://10.0.0.5:8080",
		"http://[::1]:8080/path",
		"http://[fd00::a1]",
		"http://host",
	} {
		if err := validateServiceURL(u, nil); err != nil {
			t.Errorf("%q: %v", u, err)
		}
	}
}

func TestValidateServiceURLRejects(t *testing.T) {
	cases := []struct{ url, want string }{
		{"", "empty"},
		{"cats.example.svc", "missing scheme"},
		{"ftp://cats.example.svc", "scheme"},
		{"javascript://cats.example.svc", "scheme"},
		{"file:///etc/passwd", "scheme"},
		{"http://", "missing host"},
		{"http:///path", "missing host"},
		{"http://user@cats.example.svc", "userinfo"},
		{"http://user:pw@cats.example.svc", "userinfo"},
		{"http://cats.example.svc@evil.example/", "userinfo"},
		{"http://cats.example.svc:", "invalid port"},
		{"http://cats.example.svc:0", "invalid port"},
		{"http://cats.example.svc:65536", "invalid port"},
		{"http://cats.example.svc:8o80", "invalid port"},
		{"http://cats.example.svc:+80", "invalid port"},
		{"http://a:1:2", "invalid port"},
		{"http://cats..example.svc", "invalid host"},
		{"http://cats.example.svc.", "invalid host"},
		{"http://.example.svc", "invalid host"},
		{"http://cats_x.example.svc", "invalid host"},
		{"http://[::1", "unterminated"},
		{"http://[]", "invalid IPv6"},
		{"http://[::1]x", "after IPv6"},
		{"http://[zz::1]", "invalid IPv6"},
		{"http://cats.example.svc/a b", "whitespace"},
		{"http://cats.example.svc\r\nx-bad: 1", "whitespace"},
		{"http://cäts.example.svc", "non-ASCII"},
		{"http://" + strings.Repeat("a", maxURLLen) + ".example.svc", "longer than"},
	}
	for _, c := range cases {
		err := validateServiceURL(c.url, nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want %q", c.url, err, c.want)
		}
	}
}

// allow parses allowedHosts entries as New does when clients do not supply
// targets.
func allow(t *testing.T, entries ...string) *allowlist {
	t.Helper()
	list, err := parseAllowedHosts(entries, false)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestValidateServiceURLAllowlist(t *testing.T) {
	allowed := allow(t, ".example.svc", "ha.lan", "[::1]")
	for _, u := range []string{
		"http://cats.example.svc",
		"https://a.b.example.svc:8443/x",
		"http://CATS.EXAMPLE.SVC",
		"http://ha.lan:8123",
		"http://[::1]:8080",
		"http://[0:0:0:0:0:0:0:1]", // another spelling of the same address
	} {
		if err := validateServiceURL(u, allowed); err != nil {
			t.Errorf("%q: %v", u, err)
		}
	}
	for _, u := range []string{
		"http://example.svc",               // the bare domain is not under it
		"http://evilexample.svc",           // no dot boundary
		"http://cats.example.svc.evil",     // suffix in the middle
		"http://example.svc.cluster.local", // sibling zone
		"http://sub.ha.lan",                // exact entry, not a suffix
		"http://10.0.0.5",
		"http://[::2]",
	} {
		err := validateServiceURL(u, allowed)
		if err == nil || !strings.Contains(err.Error(), "not in allowedHosts") {
			t.Errorf("%q: got %v", u, err)
		}
	}
}

// An entry with a port admits that port only. The port compared is the one
// that would be dialled, so a URL without one has its scheme's.
func TestValidateServiceURLAllowlistPorts(t *testing.T) {
	allowed := allow(t, "sut:80", ".siding-sut.svc:8080", "[::1]:9000", "10.0.0.5:443", "any.lan")
	for _, u := range []string{
		"http://sut",
		"http://sut:80/path",
		"http://sut:080", // the same port, written differently
		"https://sut:80", // the scheme is not part of an entry
		"http://cats.siding-sut.svc:8080",
		"https://cats.siding-sut.svc:8080",
		"http://[::1]:9000",
		"http://[0::1]:9000",
		"https://10.0.0.5",
		"http://any.lan:1234", // no port in the entry: any port
		"https://any.lan",
	} {
		if err := validateServiceURL(u, allowed); err != nil {
			t.Errorf("%q: %v", u, err)
		}
	}
	for u, want := range map[string]string{
		"http://sut:8081":                 `"sut:8081" is not in allowedHosts`,
		"https://sut":                     `"sut:443" is not in allowedHosts`,
		"http://cats.siding-sut.svc":      `"cats.siding-sut.svc:80" is not in allowedHosts`,
		"http://cats.siding-sut.svc:5432": `"cats.siding-sut.svc:5432" is not in allowedHosts`,
		"http://[::1]":                    `"[::1]:80" is not in allowedHosts`,
		"http://10.0.0.5":                 `"10.0.0.5:80" is not in allowedHosts`,
		"http://siding-sut.svc:8080":      `"siding-sut.svc:8080" is not in allowedHosts`,
	} {
		err := validateServiceURL(u, allowed)
		if err == nil || err.Error() != want {
			t.Errorf("%q: got %v, want %s", u, err, want)
		}
	}
}

// What stands between brackets has to be an IPv6 address, not merely be
// spelt with its alphabet.
func TestValidateServiceURLRejectsFalseIPv6(t *testing.T) {
	for _, u := range []string{
		"http://[abc]",
		"http://[1.2.3.4]:8080",
		"http://[::1::2]",
		"http://[:]",
	} {
		err := validateServiceURL(u, nil)
		if err == nil || !strings.Contains(err.Error(), "invalid IPv6 literal") {
			t.Errorf("%q: got %v", u, err)
		}
	}
}

func TestAllowlistNilVersusEmpty(t *testing.T) {
	if validateServiceURL("http://cats.example.svc", &allowlist{}) == nil {
		t.Error("empty allowlist admitted a host")
	}
	if err := validateServiceURL("http://cats.example.svc", nil); err != nil {
		t.Errorf("nil allowlist: %v", err)
	}
}

func TestDecodeOverridesAllowlistRejectsWholePayload(t *testing.T) {
	allowed := allow(t, ".example.svc")
	_, err := decodeOverrides(encodePayload(`{"services":[
		{"name":"cats","url":"http://cats.example.svc"},
		{"name":"evil","url":"http://evil.example"}]}`), allowed)
	if err == nil || !strings.Contains(err.Error(), "evil") {
		t.Fatalf("got %v", err)
	}
	if _, err := decodeOverrides(encodePayload(`{"services":[{"name":"cats","url":"http://cats.example.svc:8080/v1"}]}`), allowed); err != nil {
		t.Fatal(err)
	}
}
