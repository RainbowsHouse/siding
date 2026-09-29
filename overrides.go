package siding

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Limits on the routing-overrides payload. They bound what a request can
// make the middleware hold and inject.
const (
	maxServices   = 16
	maxURLLen     = 2048
	maxServiceLen = 32
)

// Service is one routing override: requests for Name go to URL.
type Service struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// Overrides is the routing-overrides payload, carried in baggage as
// base64(JSON).
type Overrides struct {
	Services []Service `json:"services"`
}

// lookup returns the URL for service; the first entry wins on duplicates.
func (o *Overrides) lookup(service string) (string, bool) {
	for _, s := range o.Services {
		if s.Name == service {
			return s.URL, true
		}
	}
	return "", false
}

// encode renders the payload for baggage: unpadded URL-safe base64, whose
// alphabet is all baggage-octets, so it needs no percent-encoding.
func (o *Overrides) encode() string {
	data, _ := json.Marshal(o)
	return base64.RawURLEncoding.EncodeToString(data)
}

// decodeOverrides decodes and validates a client-supplied payload. Any
// base64 flavour is accepted. A single invalid entry rejects the whole
// payload: applying part of it would let a request smuggle some overrides
// past validation.
func decodeOverrides(encoded string, allowed *allowlist) (*Overrides, error) {
	data, err := decodeBase64(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	var o Overrides
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	if err := o.validate(allowed); err != nil {
		return nil, err
	}
	return &o, nil
}

func decodeBase64(s string) ([]byte, error) {
	encodings := []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	}
	var firstErr error
	for _, enc := range encodings {
		data, err := enc.DecodeString(s)
		if err == nil {
			return data, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, firstErr
}

// validate checks every entry. A nil allowlist admits any target; one with
// no entries admits none.
func (o *Overrides) validate(allowed *allowlist) error {
	if len(o.Services) > maxServices {
		return fmt.Errorf("too many services: %d (max %d)", len(o.Services), maxServices)
	}
	for _, s := range o.Services {
		if err := validateServiceName(s.Name); err != nil {
			return err
		}
		if err := validateServiceURL(s.URL, allowed); err != nil {
			return fmt.Errorf("invalid URL for service %q: %w", s.Name, err)
		}
	}
	return nil
}

func validateServiceName(name string) error {
	ok := name != "" && len(name) <= maxServiceLen
	for i := 0; ok && i < len(name); i++ {
		c := name[i]
		ok = (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-'
	}
	if !ok {
		return fmt.Errorf("invalid service name %q: must match [a-z0-9-]{1,%d}", name, maxServiceLen)
	}
	return nil
}

// validateServiceURL accepts http(s) URLs with no userinfo, a hostname or IP
// literal host, an optional port, and — when allowed is non-nil — an
// endpoint the allowlist admits. It parses by hand rather than with net/url,
// which is far more permissive than a routing target should be.
func validateServiceURL(raw string, allowed *allowlist) error {
	host, port, err := serviceURLHost(raw)
	if err != nil {
		return err
	}
	if allowed != nil && !allowed.admits(host, port) {
		return fmt.Errorf("%q is not in allowedHosts", endpoint(host, port))
	}
	return nil
}

// serviceURLHost checks raw's shape and returns the endpoint it names: the
// host in canonicalHost's form, and the port it would be dialled on, which
// is the scheme's when the URL has none.
func serviceURLHost(raw string) (host string, port int, err error) {
	if raw == "" {
		return "", 0, errors.New("empty")
	}
	if len(raw) > maxURLLen {
		return "", 0, fmt.Errorf("longer than %d bytes", maxURLLen)
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= ' ' || raw[i] > '~' {
			return "", 0, errors.New("contains whitespace, control or non-ASCII characters")
		}
	}
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return "", 0, errors.New("missing scheme")
	}
	port = 80
	if strings.EqualFold(scheme, "https") {
		port = 443
	} else if !strings.EqualFold(scheme, "http") {
		return "", 0, fmt.Errorf("scheme %q is not http or https", scheme)
	}
	authority := rest
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		authority = rest[:i]
	}
	if authority == "" {
		return "", 0, errors.New("missing host")
	}
	if strings.Contains(authority, "@") {
		return "", 0, errors.New("userinfo is not allowed")
	}
	rawHost, rawPort, hasPort, err := splitHostPort(authority)
	if err != nil {
		return "", 0, err
	}
	if host, _, err = canonicalHost(rawHost); err != nil {
		return "", 0, err
	}
	if hasPort {
		if port, err = parsePort(rawPort); err != nil {
			return "", 0, err
		}
	}
	return host, port, nil
}

// splitHostPort splits an authority (no userinfo) into host and optional
// port and checks the host's shape. IPv6 literals keep their brackets.
func splitHostPort(authority string) (host, port string, hasPort bool, err error) {
	if strings.HasPrefix(authority, "[") {
		end := strings.IndexByte(authority, ']')
		if end < 0 {
			return "", "", false, errors.New("unterminated IPv6 literal")
		}
		inner := authority[1:end]
		if inner == "" || strings.Trim(inner, "0123456789abcdefABCDEF:.") != "" {
			return "", "", false, fmt.Errorf("invalid IPv6 literal %q", inner)
		}
		host, tail := authority[:end+1], authority[end+1:]
		if tail == "" {
			return host, "", false, nil
		}
		if !strings.HasPrefix(tail, ":") {
			return "", "", false, fmt.Errorf("unexpected %q after IPv6 literal", tail)
		}
		return host, tail[1:], true, nil
	}
	host, port, hasPort = strings.Cut(authority, ":")
	if host == "" {
		return "", "", false, errors.New("missing host")
	}
	// Hostnames and IPv4 literals share one alphabet; empty labels are
	// rejected so "a..b" or a trailing "." can't spell an allowed host
	// differently.
	for _, label := range strings.Split(host, ".") {
		if label == "" || strings.Trim(label, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") != "" {
			return "", "", false, fmt.Errorf("invalid host %q", host)
		}
	}
	return host, port, hasPort, nil
}
