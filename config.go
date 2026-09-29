package siding

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRefreshInterval = 10 * time.Second
	minRefreshInterval     = time.Second
)

// settings is a Config after validation: everything a request needs is
// parsed once, in New.
type settings struct {
	allowedHosts *allowlist      // nil when none are configured
	trusted      []netip.Prefix  // empty: every peer is trusted
	source       *registrySource // nil: no registry
}

// validateConfig checks cfg and parses it. Every error names the key, and
// the value where there is one, so Traefik's log says what to fix.
func validateConfig(cfg *Config) (*settings, error) {
	if cfg.Service == "" {
		return nil, errors.New("service is required: the name this middleware's backend has in the registry and in routing overrides")
	}
	if err := validateServiceName(cfg.Service); err != nil {
		return nil, fmt.Errorf("service: %w", err)
	}
	if err := firstError(
		checkToken("baggageHeader", cfg.BaggageHeader, true),
		checkToken("tenancyKey", cfg.TenancyKey, true),
		checkToken("overridesKey", cfg.OverridesKey, true),
		checkToken("userHeader", cfg.UserHeader, false),
		checkToken("debugHeader", cfg.DebugHeader, false),
	); err != nil {
		return nil, err
	}
	if err := firstError(
		checkHeader("baggageHeader", cfg.BaggageHeader),
		checkHeader("userHeader", cfg.UserHeader),
		checkHeader("debugHeader", cfg.DebugHeader),
		// Header names are case-insensitive, and the routing keys are
		// stripped in any case, so that is how all of them are compared.
		checkDistinct("baggageHeader", cfg.BaggageHeader, "userHeader", cfg.UserHeader),
		checkDistinct("baggageHeader", cfg.BaggageHeader, "debugHeader", cfg.DebugHeader),
		checkDistinct("userHeader", cfg.UserHeader, "debugHeader", cfg.DebugHeader),
		checkDistinct("tenancyKey", cfg.TenancyKey, "overridesKey", cfg.OverridesKey),
	); err != nil {
		return nil, err
	}
	if cfg.TenancyPrefix == "" {
		return nil, errors.New("tenancyPrefix must not be empty: it is what keeps production tenancies from being routed")
	}

	set := &settings{}
	var err error
	if set.allowedHosts, err = parseAllowedHosts(cfg.AllowedHosts, cfg.AllowHeaderOverrides); err != nil {
		return nil, err
	}
	if cfg.AllowHeaderOverrides && set.allowedHosts == nil {
		return nil, errors.New("allowHeaderOverrides requires allowedHosts: targets a client supplies must be bounded")
	}
	if set.trusted, err = parseTrustedSources(cfg.TrustedSources); err != nil {
		return nil, err
	}
	if set.source, err = parseRegistrySource(cfg.Registry, cfg.TenancyPrefix); err != nil {
		return nil, err
	}
	if cfg.UserHeader != "" && set.source == nil {
		return nil, fmt.Errorf("userHeader %q needs a registry: accounts are looked up there", cfg.UserHeader)
	}
	if set.source == nil && !cfg.AllowHeaderOverrides {
		return nil, errors.New("nothing can route: set registry.file, registry.url or registry.tenancies, or allowHeaderOverrides with allowedHosts")
	}
	return set, nil
}

func firstError(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// checkToken checks a header name or baggage key, both RFC 9110 tokens.
func checkToken(key, value string, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("%s must not be empty", key)
		}
		return nil
	}
	if !isToken(value) {
		return fmt.Errorf("%s %q: not a valid name: use letters, digits and !#$%%&'*+-.^_`|~ only", key, value)
	}
	return nil
}

// reservedHeaders are headers with a meaning of their own to HTTP, to Traefik
// or to the backend: reading routing from one, or writing the debug value
// over one, breaks the request. Lower-cased.
var reservedHeaders = map[string]bool{
	"authorization": true, "connection": true, "content-encoding": true,
	"content-length": true, "content-type": true, "cookie": true,
	"host": true, "location": true, "proxy-authorization": true,
	"set-cookie": true, "te": true, "trailer": true,
	"transfer-encoding": true, "upgrade": true,
	"x-forwarded-for": true, "x-forwarded-host": true, "x-forwarded-proto": true,
}

func checkHeader(key, value string) error {
	if reservedHeaders[strings.ToLower(value)] {
		return fmt.Errorf("%s %q: that header has a meaning of its own; choose one this middleware can have to itself", key, value)
	}
	return nil
}

func checkDistinct(keyA, a, keyB, b string) error {
	if a != "" && strings.EqualFold(a, b) {
		return fmt.Errorf("%s and %s are both %q: they must differ", keyA, keyB, a)
	}
	return nil
}

// parseRefreshInterval reads a Go duration, or a bare number of seconds as
// Traefik's own durations allow. Empty is the default.
func parseRefreshInterval(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultRefreshInterval, nil
	}
	var interval time.Duration
	if strings.Trim(raw, "0123456789") == "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds > 24*60*60 {
			return 0, fmt.Errorf("registry.refreshInterval %q: too large", raw)
		}
		interval = time.Duration(seconds) * time.Second
	} else {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return 0, fmt.Errorf(`registry.refreshInterval %q: not a duration: use a Go duration ("10s", "1m") or a number of seconds`, raw)
		}
		interval = d
	}
	if interval < minRefreshInterval {
		return 0, fmt.Errorf("registry.refreshInterval %q: must be at least %s", raw, minRefreshInterval)
	}
	return interval, nil
}

// parseRegistrySource works out which registry cfg describes: nil when it
// names none. An inline registry is decoded and validated here. prefix is
// the tenancyPrefix the registry's entries are checked against.
func parseRegistrySource(cfg *RegistryConfig, prefix string) (*registrySource, error) {
	if cfg == nil {
		return nil, nil
	}
	interval, err := parseRefreshInterval(cfg.RefreshInterval)
	if err != nil {
		return nil, err
	}
	inline := isSet(cfg.Tenancies) || isSet(cfg.Accounts)
	var given []string
	if inline {
		given = append(given, "inline tenancies/accounts")
	}
	if cfg.File != "" {
		given = append(given, "file")
	}
	if cfg.URL != "" {
		given = append(given, "url")
	}
	if len(given) > 1 {
		return nil, fmt.Errorf("registry: set one of inline tenancies/accounts, file or url, not %s", strings.Join(given, " and "))
	}
	if len(given) == 0 {
		return nil, nil
	}
	if inline {
		snap, err := decodeInlineRegistry(cfg, prefix)
		if err != nil {
			return nil, fmt.Errorf("registry (inline): %w", err)
		}
		return &registrySource{kind: sourceInline, inline: snap}, nil
	}
	if cfg.File != "" {
		return &registrySource{kind: sourceFile, location: cfg.File, interval: interval, prefix: prefix}, nil
	}
	if _, _, err := serviceURLHost(cfg.URL); err != nil {
		return nil, fmt.Errorf("registry.url %q: %w", cfg.URL, err)
	}
	return &registrySource{kind: sourceURL, location: cfg.URL, interval: interval, prefix: prefix}, nil
}

// isSet reports whether an inline registry value was given. Traefik's file
// provider hands an empty map over as "".
func isSet(v interface{}) bool {
	if v == nil {
		return false
	}
	if s, ok := v.(string); ok {
		return s != ""
	}
	if m, ok := v.(map[string]interface{}); ok {
		return len(m) > 0
	}
	return true
}

// decodeInlineRegistry runs the inline values through the registry
// document's own decoder, so inline and file registries accept exactly the
// same thing. What a polled registry only warns about is an error here: the
// configuration is the operator's own and there is no last good snapshot to
// keep.
func decodeInlineRegistry(cfg *RegistryConfig, prefix string) (*snapshot, error) {
	doc := map[string]interface{}{}
	if isSet(cfg.Tenancies) {
		doc["tenancies"] = cfg.Tenancies
	}
	if isSet(cfg.Accounts) {
		doc["accounts"] = cfg.Accounts
	}
	data, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	snap, warnings, err := decodeRegistry(data, prefix)
	if err != nil {
		return nil, err
	}
	if len(warnings) > 0 {
		return nil, errors.New(warnings[0])
	}
	return snap, nil
}
