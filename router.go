// Package siding is a Traefik middleware plugin for request-level test
// routing, after Uber's SLATE
// (https://www.uber.com/blog/simplifying-developer-testing-through-slate/).
// A siding is the track beside the main line that a train is switched onto:
// test requests travel the production route, and are switched off it only
// where a service under test stands in for the real one.
//
// A test request carries a tenancy in W3C baggage (request-tenancy=test/…).
// The middleware looks that tenancy up in a registry of services-under-test
// (SUTs), and when the registry, or an explicit and allowlisted
// routing-overrides baggage entry, names the service this middleware guards,
// the request is proxied to the SUT instead of the normal backend. Resolved
// routing is written back into the baggage so later hops that pass through
// Traefik route the same way without their own lookup. Anything unresolved
// falls through to the normal backend.
package siding

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"strings"
	"sync"
)

// Traefik hands Yaegi two writers into its own logger, one at DEBUG and one
// at ERROR (pkg/plugins/middlewareyaegi.go), and Yaegi points only
// fmt.Print* at the first and the log package's own functions at the second.
// os.Stdout and os.Stderr stay the process's streams: a logger built on them
// writes raw lines past Traefik's level, format and timestamps. So what an
// operator has to see goes through log.Print, and the rest through
// fmt.Print.
//
// The sinks are variables so tests can read what was logged.
var (
	sinkMu   sync.RWMutex
	infoSink = func(line string) { fmt.Println(line) }
	warnSink = func(line string) { log.Println(line) }
)

func init() {
	// Traefik stamps each entry itself.
	log.SetFlags(0)
}

const logPrefix = "[siding] "

func infof(format string, args ...interface{}) {
	sinkMu.RLock()
	sink := infoSink
	sinkMu.RUnlock()
	sink(logPrefix + fmt.Sprintf(format, args...))
}

func warnf(format string, args ...interface{}) {
	sinkMu.RLock()
	sink := warnSink
	sinkMu.RUnlock()
	sink(logPrefix + fmt.Sprintf(format, args...))
}

// maxDebugValue bounds the debug header: its reasons quote what the client
// sent.
const maxDebugValue = 256

// maxDroppedWarnings bounds how many distinct dropped targets a Router
// remembers having warned about.
const maxDroppedWarnings = 64

// Config is the middleware configuration; see README.md.
type Config struct {
	// Service is the logical name of the backend this middleware sits in
	// front of: the key looked up in a tenancy's registry services and in
	// routing overrides. It must match [a-z0-9-]{1,32}.
	Service string `json:"service,omitempty"`

	BaggageHeader string `json:"baggageHeader,omitempty"`
	TenancyKey    string `json:"tenancyKey,omitempty"`
	OverridesKey  string `json:"overridesKey,omitempty"`
	// TenancyPrefix limits routing to test tenancies: a request whose
	// tenancy is outside it is never routed, whatever else it carries.
	TenancyPrefix string `json:"tenancyPrefix,omitempty"`
	// UserHeader names a request header (for example set by forwardAuth)
	// whose value is looked up in the registry's accounts when the baggage
	// has no tenancy. Empty disables account binding.
	UserHeader string `json:"userHeader,omitempty"`

	Registry *RegistryConfig `json:"registry,omitempty"`

	// AllowHeaderOverrides trusts explicit routing-overrides baggage. When
	// false the entry is stripped, so a downstream hop that does trust it
	// never sees a client-forged one.
	AllowHeaderOverrides bool `json:"allowHeaderOverrides,omitempty"`
	// AllowedHosts restricts SUT targets: "host" or ".domain", each with an
	// optional ":port". Required with AllowHeaderOverrides, and then every
	// entry names its port; when set it also applies to registry targets.
	AllowedHosts []string `json:"allowedHosts,omitempty"`
	// TrustedSources lists the peers, as CIDRs or addresses, whose routing
	// baggage is accepted. From any other peer the tenancy and overrides
	// entries are removed and only UserHeader can select a tenancy. Empty
	// trusts every peer.
	TrustedSources []string `json:"trustedSources,omitempty"`

	InjectBaggage bool `json:"injectBaggage,omitempty"`
	// DebugHeader, when set, is added to responses as
	// "routed=<url>" or "fallback=<reason>".
	DebugHeader string `json:"debugHeader,omitempty"`
}

// CreateConfig returns the default configuration.
func CreateConfig() *Config {
	return &Config{
		BaggageHeader: "baggage",
		TenancyKey:    "request-tenancy",
		OverridesKey:  "routing-overrides",
		TenancyPrefix: "test/",
		Registry:      &RegistryConfig{RefreshInterval: defaultRefreshInterval.String()},
		InjectBaggage: true,
	}
}

// Router is the middleware handler.
type Router struct {
	next     http.Handler
	name     string
	cfg      *Config
	registry *registry

	allowedHosts *allowlist     // nil: registry targets are unrestricted
	trusted      []netip.Prefix // empty: every peer is trusted

	droppedMu sync.Mutex
	dropped   map[string]bool

	failures *failureLog
}

// New builds the middleware. Invalid configuration is an error, so Traefik
// reports it instead of the middleware silently doing nothing.
func New(ctx context.Context, next http.Handler, cfg *Config, name string) (http.Handler, error) {
	if cfg == nil {
		return nil, errors.New(name + ": no configuration")
	}
	set, err := validateConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	r := &Router{
		next:         next,
		name:         name,
		cfg:          cfg,
		allowedHosts: set.allowedHosts,
		trusted:      set.trusted,
		dropped:      map[string]bool{},
		failures:     newFailureLog(),
	}
	source := "none"
	if set.source != nil {
		// Last, after everything that can fail: a registry is held until
		// ctx is cancelled.
		r.registry = openRegistry(ctx, set.source)
		source = set.source.describe()
	}
	infof("%s: service=%s registry=%s headerOverrides=%v allowedHosts=%d trustedSources=%d injectBaggage=%v",
		name, cfg.Service, source, cfg.AllowHeaderOverrides, len(cfg.AllowedHosts), len(r.trusted), cfg.InjectBaggage)
	return r, nil
}

// decision is where a request goes: target is the SUT URL, or empty for the
// normal backend, with fallback saying why when the request looked like a
// test request.
type decision struct {
	target   string
	fallback string
	trusted  bool
}

func (r *Router) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	d := r.resolve(req)
	if d.target != "" {
		r.setDebug(rw, d, "routed="+d.target)
		target := d.target
		serveProxy(rw, req, target, func(err error) { r.proxyFailed(target, err) })
		return
	}
	if d.fallback != "" {
		r.setDebug(rw, d, "fallback="+d.fallback)
	}
	r.next.ServeHTTP(rw, req)
}

// proxyFailed logs a request the SUT did not answer.
func (r *Router) proxyFailed(target string, err error) {
	if errors.Is(err, context.Canceled) {
		// The client went away; nothing is wrong with the SUT.
		infof("%s: proxy to %s: %v", r.name, target, err)
		return
	}
	log, unreported := r.failures.report(target)
	if !log {
		return
	}
	if unreported > 0 {
		warnf("%s: proxy to %s: %v (and %d more failures since the last one reported)", r.name, target, err, unreported)
		return
	}
	warnf("%s: proxy to %s: %v", r.name, target, err)
}

// setDebug adds the debug header, except for a peer outside trustedSources:
// it names internal hosts.
func (r *Router) setDebug(rw http.ResponseWriter, d decision, value string) {
	if r.cfg.DebugHeader == "" || !d.trusted {
		return
	}
	if len(value) > maxDebugValue {
		// Reasons are ASCII (%+q), so any byte is a place to cut.
		value = value[:maxDebugValue-3] + "..."
	}
	rw.Header().Set(r.cfg.DebugHeader, value)
}

// resolve works out where req should go, rewriting its baggage on the way.
func (r *Router) resolve(req *http.Request) decision {
	d := decision{trusted: r.trustedPeer(req)}

	values := req.Header.Values(r.cfg.BaggageHeader)
	bag, err := parseBaggage(values)
	oversized := err != nil
	if oversized {
		if d.trusted && !r.namesRoutingKey(values) {
			// Leave an oversized header as it is: rewriting it would drop
			// other systems' data.
			d.fallback = err.Error()
			return d
		}
		// One that names a routing key is dropped, as is any from an
		// untrusted peer: padding the header must not carry a routing
		// member past the strips below.
		bag = &baggage{changed: true}
	}
	if d.trusted {
		bag.keepFirst(r.cfg.TenancyKey)
		bag.keepFirst(r.cfg.OverridesKey)
	} else {
		bag.delFold(r.cfg.TenancyKey)
		bag.delFold(r.cfg.OverridesKey)
		if bag.changed {
			infof("%s: routing baggage from %s, which is not in trustedSources, removed", r.name, req.RemoteAddr)
		}
	}

	tenancy := r.resolveTenancy(bag, req)
	if tenancy != "" && !strings.HasPrefix(tenancy, r.cfg.TenancyPrefix) {
		// A tenancy outside the prefix is never routed, so its overrides
		// go no further either.
		bag.delFold(r.cfg.OverridesKey)
		r.writeBaggage(req, bag)
		d.fallback = fmt.Sprintf("tenancy %+q is not under %q", tenancy, r.cfg.TenancyPrefix)
		return d
	}

	overrides, unresolved := r.resolveOverrides(bag, tenancy)
	r.inject(bag, tenancy, overrides)
	r.writeBaggage(req, bag)

	if overrides != nil {
		if url, ok := overrides.lookup(r.cfg.Service); ok {
			d.target = url
			return d
		}
	}
	d.fallback = r.explain(overrides, unresolved, oversized)
	return d
}

// explain says why a request that was not routed wasn't, or "" when it
// never looked like a test request. unresolved is resolveOverrides' account
// of what it could not use.
func (r *Router) explain(overrides *Overrides, unresolved string, oversized bool) string {
	if unresolved != "" {
		return unresolved
	}
	if overrides != nil {
		return "no override for service " + r.cfg.Service
	}
	if oversized {
		return errBaggageTooLarge.Error()
	}
	return ""
}

// namesRoutingKey reports whether a baggage header mentions the tenancy or
// the overrides key, in any case. It looks for the text rather than parsing:
// the header is one the parser refused.
func (r *Router) namesRoutingKey(values []string) bool {
	header := strings.ToLower(strings.Join(values, ","))
	return strings.Contains(header, strings.ToLower(r.cfg.TenancyKey)) ||
		strings.Contains(header, strings.ToLower(r.cfg.OverridesKey))
}

// resolveTenancy reads the tenancy from baggage, or from the registry's
// account bindings via UserHeader. It returns "" when the request names
// none; whether the one it names may be routed is the caller's to decide.
func (r *Router) resolveTenancy(bag *baggage, req *http.Request) string {
	if tenancy, _ := bag.get(r.cfg.TenancyKey); tenancy != "" {
		return tenancy
	}
	if r.cfg.UserHeader == "" || r.registry == nil {
		return ""
	}
	user := req.Header.Get(r.cfg.UserHeader)
	if user == "" {
		return ""
	}
	tenancy, _ := r.registry.account(user)
	return tenancy
}

// resolveOverrides prefers explicit baggage overrides (when trusted), then
// the registry entry for tenancy. Rejected header overrides are stripped
// from the baggage.
//
// unresolved is what it could not use, for the debug header. It is not a
// verdict: overrides may come back with it, when the registry made up for
// rejected header overrides or when the allowlist left a target out. What
// the registry has to say replaces what was said of the header.
func (r *Router) resolveOverrides(bag *baggage, tenancy string) (overrides *Overrides, unresolved string) {
	rejected := ""
	if raw, ok := bag.get(r.cfg.OverridesKey); ok {
		if !r.cfg.AllowHeaderOverrides {
			rejected = "header overrides are not allowed"
		} else if o, err := decodeOverrides(raw, r.allowedHosts); err != nil {
			rejected = "invalid header overrides: " + err.Error()
		} else {
			return o, ""
		}
		bag.delFold(r.cfg.OverridesKey)
	}

	if tenancy == "" {
		return nil, rejected
	}
	if r.registry == nil {
		return nil, firstNonEmpty(rejected, "no registry configured")
	}
	entry, missing := r.registry.tenancy(tenancy)
	if entry == nil {
		return nil, firstNonEmpty(rejected, missing)
	}
	return r.view(tenancy, entry)
}

// view is a tenancy's services as this middleware may route them. A target
// outside allowedHosts is left out — of the lookup and of what is injected,
// since a later hop that trusts header overrides rejects a payload whole for
// one such entry — rather than costing the tenancy its other services.
// dropped says so when it is this middleware's own target that was left out.
func (r *Router) view(name string, entry *tenancy) (overrides *Overrides, dropped string) {
	overrides = &Overrides{Services: make([]Service, 0, len(entry.targets))}
	for _, t := range entry.targets {
		if r.allowedHosts != nil && !r.allowedHosts.admits(t.host, t.port) {
			r.warnDropped(name, t)
			if t.name == r.cfg.Service {
				dropped = fmt.Sprintf("tenancy %+q: %q for service %q is not in allowedHosts", name, endpoint(t.host, t.port), t.name)
			}
			continue
		}
		overrides.Services = append(overrides.Services, Service{Name: t.name, URL: t.url})
	}
	return overrides, dropped
}

// warnDropped logs a registry target the allowlist rejects, once.
func (r *Router) warnDropped(name string, t target) {
	key := name + "|" + t.name + "|" + t.url
	r.droppedMu.Lock()
	defer r.droppedMu.Unlock()
	if r.dropped[key] {
		return
	}
	if len(r.dropped) >= maxDroppedWarnings {
		r.dropped = map[string]bool{}
	}
	r.dropped[key] = true
	warnf("%s: tenancy %q: %s for service %q is ignored: %q is not in allowedHosts", r.name, name, t.url, t.name, endpoint(t.host, t.port))
}

// inject writes resolved routing into the baggage for later hops, as far as
// the W3C limits allow: a list pushed past them is one the next hop refuses
// whole, which would send the request's later hops to production. So it
// writes the tenancy and the overrides, or else the tenancy alone, or else
// nothing. The overrides are not given up when they arrived with the
// request: they are a peer's, and the tenancy is only ours to add.
//
// A member that already says the same is left as it is, properties and all.
func (r *Router) inject(bag *baggage, tenancy string, overrides *Overrides) {
	if !r.cfg.InjectBaggage {
		return
	}
	_, arrived := bag.get(r.cfg.OverridesKey)

	full := bag.clone()
	setIfDifferent(full, r.cfg.TenancyKey, tenancy)
	if overrides != nil && len(overrides.Services) > 0 {
		setIfDifferent(full, r.cfg.OverridesKey, overrides.encode())
	}
	if full.fits() {
		*bag = *full
		return
	}
	if !arrived {
		tenancyOnly := bag.clone()
		setIfDifferent(tenancyOnly, r.cfg.TenancyKey, tenancy)
		if tenancyOnly.fits() {
			*bag = *tenancyOnly
			infof("%s: baggage is near the W3C limits: tenancy %+q injected without its overrides", r.name, tenancy)
			return
		}
	}
	infof("%s: baggage is at the W3C limits: routing for tenancy %+q not injected", r.name, tenancy)
}

// setIfDifferent sets key unless value is empty or the list says so already.
func setIfDifferent(bag *baggage, key, value string) {
	if value == "" {
		return
	}
	if current, ok := bag.get(key); !ok || current != value {
		bag.set(key, value)
	}
}

func (r *Router) writeBaggage(req *http.Request, bag *baggage) {
	if !bag.changed {
		return
	}
	if value := bag.String(); value != "" {
		req.Header.Set(r.cfg.BaggageHeader, value)
		return
	}
	req.Header.Del(r.cfg.BaggageHeader)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
