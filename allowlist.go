package siding

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// allowlist is allowedHosts, parsed: the endpoints a SUT target may name. A
// nil *allowlist admits anything, which callers check for themselves; one
// with no entries admits nothing.
type allowlist struct {
	entries []allowedHost
}

// allowedHost is one allowedHosts entry.
type allowedHost struct {
	// host is lower-cased, and an IP literal is in its canonical form, IPv6
	// in brackets. For a suffix entry it is the domain, without the leading
	// dot.
	host string
	// suffix admits every host under the domain, not the domain itself.
	suffix bool
	// port is 0 for any port.
	port int
}

// admits reports whether an entry covers host and port, both as
// serviceURLHost returns them. The scheme plays no part: an entry names a
// network endpoint.
func (a *allowlist) admits(host string, port int) bool {
	for _, e := range a.entries {
		if e.port != 0 && e.port != port {
			continue
		}
		if e.suffix {
			if strings.HasSuffix(host, "."+e.host) {
				return true
			}
			continue
		}
		if host == e.host {
			return true
		}
	}
	return false
}

// parseAllowedHosts reads allowedHosts. With needPort every entry has to
// name its port: that is the case when clients supply targets, since an
// entry without one would let them reach every port on the host. It returns
// nil for an empty list.
func parseAllowedHosts(entries []string, needPort bool) (*allowlist, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	list := &allowlist{entries: make([]allowedHost, 0, len(entries))}
	for i, entry := range entries {
		parsed, err := parseAllowedHost(strings.TrimSpace(entry), needPort)
		if err != nil {
			return nil, fmt.Errorf("allowedHosts[%d] %q: %w", i, entry, err)
		}
		list.entries = append(list.entries, parsed)
	}
	return list, nil
}

func parseAllowedHost(entry string, needPort bool) (allowedHost, error) {
	const forms = `use a host ("cats.siding-sut.svc:8080") or a leading dot for every host under a domain (".siding-sut.svc:8080")`
	none := allowedHost{}
	if entry == "" {
		return none, errors.New("empty entry; " + forms)
	}
	if strings.Contains(entry, "*") {
		return none, errors.New("wildcards are not supported; " + forms)
	}
	if strings.ContainsAny(entry, "/?#@") {
		return none, errors.New("not a host; " + forms)
	}
	name := strings.TrimPrefix(entry, ".")
	suffix := name != entry
	if !strings.HasPrefix(name, "[") && strings.Count(name, ":") > 1 {
		return none, errors.New(`IPv6 literals are written in brackets, as in "[::1]:8080"`)
	}
	rawHost, rawPort, hasPort, err := splitHostPort(name)
	if err != nil {
		return none, fmt.Errorf("%w; %s", err, forms)
	}
	host, isIP, err := canonicalHost(rawHost)
	if err != nil {
		return none, err
	}
	if suffix && (isIP || numericLabel(host)) {
		return none, errors.New("an IP address has no hosts under it; " + forms)
	}
	parsed := allowedHost{host: host, suffix: suffix}
	if hasPort {
		if parsed.port, err = parsePort(rawPort); err != nil {
			return none, err
		}
	}
	if needPort && parsed.port == 0 {
		return none, fmt.Errorf("needs a port when allowHeaderOverrides is on, or a client could reach every port on it: write it as %q, with the port the service listens on", entry+":8080")
	}
	return parsed, nil
}

// numericLabel reports whether host's last label is all digits, as no
// domain's is: ".0.5" would match addresses, not names.
func numericLabel(host string) bool {
	last := host[strings.LastIndexByte(host, '.')+1:]
	return strings.Trim(last, "0123456789") == ""
}

// canonicalHost puts a host splitHostPort accepted into the one form
// targets and entries are compared in. An IP literal is parsed rather than
// matched by its alphabet, so "[abc]" and "[1.2.3.4]" are refused here and
// not at dial time, and every spelling of an address compares equal.
func canonicalHost(host string) (canonical string, isIP bool, err error) {
	if strings.HasPrefix(host, "[") {
		inner := strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		addr, err := netip.ParseAddr(inner)
		if err != nil || !addr.Is6() {
			return "", false, fmt.Errorf("invalid IPv6 literal %q", inner)
		}
		return "[" + addr.String() + "]", true, nil
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.String(), true, nil
	}
	return strings.ToLower(host), false, nil
}

// parsePort reads a port as a number, so "080" and "80" are the same port.
func parsePort(raw string) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil || len(raw) > 5 || n < 1 || n > 65535 || strings.Trim(raw, "0123456789") != "" {
		return 0, fmt.Errorf("invalid port %q", raw)
	}
	return n, nil
}

// endpoint is host and port as one string, for messages.
func endpoint(host string, port int) string {
	return host + ":" + strconv.Itoa(port)
}
