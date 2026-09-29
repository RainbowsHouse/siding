package siding

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxRegistryBytes caps how much of a registry file or response is read.
const maxRegistryBytes = 4 << 20

const (
	sourceInline = "inline"
	sourceFile   = "file"
	sourceURL    = "url"
)

// RegistryConfig says where tenancy → SUT mappings come from: a file (in k8s,
// a mounted ConfigMap), an HTTP endpoint, or the configuration itself. Set at
// most one; with none there is no registry.
type RegistryConfig struct {
	File string `json:"file,omitempty"`
	URL  string `json:"url,omitempty"`
	// RefreshInterval is how often File or URL is re-read: a Go duration or
	// a number of seconds, at least 1s.
	RefreshInterval string `json:"refreshInterval,omitempty"`

	// Tenancies and Accounts are the registry written inline, in the shape
	// of the registry document's members of the same name. They are untyped
	// because Traefik's file provider hands an empty map over as "", which
	// does not decode into a typed one.
	Tenancies interface{} `json:"tenancies,omitempty"`
	Accounts  interface{} `json:"accounts,omitempty"`
}

// registrySource is a validated RegistryConfig.
type registrySource struct {
	kind     string // sourceInline, sourceFile or sourceURL
	location string // the path or URL
	interval time.Duration
	// prefix is the tenancyPrefix of the middleware reading the registry:
	// what its entries have to be under to be of any use.
	prefix string
	inline *snapshot
}

// describe names the source in logs: "file:/path", "url:http://…", "inline".
func (s *registrySource) describe() string {
	if s.kind == sourceInline {
		return sourceInline
	}
	return s.kind + ":" + s.location
}

// registryDoc is the registry's JSON shape:
//
//	{
//	  "tenancies": {"test/env-1": {"services": {"cats-openapi": "http://…"}, "expiresAt": "…"}},
//	  "accounts":  {"tester-1": "test/env-1"}
//	}
type registryDoc struct {
	Tenancies map[string]tenancyEntry `json:"tenancies"`
	Accounts  map[string]string       `json:"accounts"`
}

type tenancyEntry struct {
	Services  map[string]string `json:"services"`
	ExpiresAt *time.Time        `json:"expiresAt,omitempty"`
}

// snapshot is a registryDoc as requests read it: validated, with each
// tenancy's services sorted once rather than on every request. It is never
// modified after it is built.
type snapshot struct {
	tenancies map[string]*tenancy
	accounts  map[string]string
}

type tenancy struct {
	// targets is sorted by service name, so the baggage it is injected into
	// is stable.
	targets   []target
	expiresAt time.Time // zero: never
}

type target struct {
	name string
	url  string
	// host and port are the endpoint url names, as the allowlist matches it.
	host string
	port int
}

// maxNamesInWarning bounds how many entries one warning lists.
const maxNamesInWarning = 5

// decodeRegistry parses and validates a registry document. Unknown members
// are an error: a misspelt "tenancies" would otherwise load as an empty
// registry. The warnings are entries that are valid but can't do anything,
// among them those outside prefix, the reader's tenancyPrefix: a tenancy
// outside it is never routed.
func decodeRegistry(data []byte, prefix string) (*snapshot, []string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc registryDoc
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, fmt.Errorf("not a registry document: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, errors.New("not a registry document: data after the closing brace")
	}

	snap := &snapshot{tenancies: map[string]*tenancy{}, accounts: doc.Accounts}
	var warnings, strayTenancies, strayAccounts []string
	names := make([]string, 0, len(doc.Tenancies))
	for name := range doc.Tenancies {
		names = append(names, name)
	}
	// Sorted, here and below, so a document with several bad entries reports
	// the same one every time.
	sort.Strings(names)
	for _, name := range names {
		if name == "" {
			return nil, nil, errors.New("tenancies: a tenancy has an empty name")
		}
		t, err := newTenancy(doc.Tenancies[name])
		if err != nil {
			return nil, nil, fmt.Errorf("tenancy %q: %w", name, err)
		}
		if len(t.targets) == 0 {
			warnings = append(warnings, fmt.Sprintf("tenancy %q has no services", name))
		}
		if !strings.HasPrefix(name, prefix) {
			strayTenancies = append(strayTenancies, name)
		}
		snap.tenancies[name] = t
	}
	for _, account := range sortedKeys(doc.Accounts) {
		bound := doc.Accounts[account]
		if account == "" || bound == "" {
			return nil, nil, fmt.Errorf("accounts: %q is bound to %q: neither may be empty", account, bound)
		}
		if !strings.HasPrefix(bound, prefix) {
			strayAccounts = append(strayAccounts, account)
		} else if _, ok := snap.tenancies[bound]; !ok {
			warnings = append(warnings, fmt.Sprintf("account %q is bound to tenancy %q, which the registry does not define", account, bound))
		}
	}
	if len(strayTenancies) > 0 {
		warnings = append(warnings, fmt.Sprintf("tenancies outside tenancyPrefix %q are never routed: %s", prefix, quoteSome(strayTenancies)))
	}
	if len(strayAccounts) > 0 {
		warnings = append(warnings, fmt.Sprintf("accounts bound to a tenancy outside tenancyPrefix %q are never routed: %s", prefix, quoteSome(strayAccounts)))
	}
	return snap, warnings, nil
}

// quoteSome lists the first few names, and how many it left out.
func quoteSome(names []string) string {
	shown := names
	if len(shown) > maxNamesInWarning {
		shown = shown[:maxNamesInWarning]
	}
	quoted := make([]string, 0, len(shown))
	for _, name := range shown {
		quoted = append(quoted, fmt.Sprintf("%+q", name))
	}
	list := strings.Join(quoted, ", ")
	if more := len(names) - len(shown); more > 0 {
		list += fmt.Sprintf(" and %d more", more)
	}
	return list
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func newTenancy(entry tenancyEntry) (*tenancy, error) {
	if len(entry.Services) > maxServices {
		return nil, fmt.Errorf("too many services: %d (max %d)", len(entry.Services), maxServices)
	}
	t := &tenancy{targets: make([]target, 0, len(entry.Services))}
	if entry.ExpiresAt != nil {
		t.expiresAt = *entry.ExpiresAt
	}
	for _, name := range sortedKeys(entry.Services) {
		if err := validateServiceName(name); err != nil {
			return nil, err
		}
		url := entry.Services[name]
		host, port, err := serviceURLHost(url)
		if err != nil {
			return nil, fmt.Errorf("invalid URL for service %q: %w", name, err)
		}
		t.targets = append(t.targets, target{name: name, url: url, host: host, port: port})
	}
	return t, nil
}

// registry holds the last successfully loaded snapshot. A failed load keeps
// the previous one, so a bad edit never drops live routes.
//
// Locks, in the order they may be taken while another is held:
//
//	first → loadMu → mu → sinkMu (the log sinks, in router.go)
//
// registriesMu is never held together with any of them: whatever needs it
// takes it, and lets it go, first.
type registry struct {
	source string
	prefix string                 // the readers' tenancyPrefix
	load   func() ([]byte, error) // nil for an inline registry
	now    func() time.Time

	// first runs the load a registry starts with. loadMu serialises
	// refresh, and guards lastData, the document snap was built from.
	first    sync.Once
	loadMu   sync.Mutex
	lastData []byte

	// mu guards what requests read.
	mu      sync.RWMutex
	snap    *snapshot
	lastErr string
	retired bool // its poller has stopped: snap will not change again

	// frozen says, once, that a retired registry is still being read.
	frozen sync.Once

	// Guarded by registriesMu.
	users     int
	idleSince time.Time
}

// Polled registries are shared per (source, interval, tenancyPrefix): Traefik
// calls New for every middleware instance and again on every dynamic-config
// reload. The prefix is part of it because a registry checks its entries
// against one: middlewares that differ in it read the same source apart. Each
// New holds its registry until its context is cancelled, which Traefik does
// at the start of the next configuration build — before it builds the new
// handlers. So a registry with no users is kept for pollerIdleGrace, and the
// rebuild finds it, and its snapshot, still there.
var (
	registriesMu    sync.Mutex
	registries      = map[string]*registry{}
	pollerIdleGrace = 30 * time.Second
)

// openRegistry returns the registry for src, held until ctx is cancelled.
func openRegistry(ctx context.Context, src *registrySource) *registry {
	if src.kind == sourceInline {
		return &registry{source: src.describe(), now: time.Now, snap: src.inline}
	}
	key := registryKey(src)

	registriesMu.Lock()
	r, ok := registries[key]
	if !ok {
		r = newRegistry(src)
		registries[key] = r
	}
	r.users++
	registriesMu.Unlock()

	// Outside the lock, so a slow URL holds up only the middlewares that
	// read it. A closure, not the method value r.refresh: Yaegi v0.16.1
	// mistypes method values handed to the standard library.
	r.first.Do(func() { r.refresh() })
	if !ok {
		go r.poll(key, src.interval)
	}
	if ctx != nil && ctx.Done() != nil {
		done := ctx.Done()
		go func() {
			<-done
			r.release()
		}()
	}
	return r
}

// registryKey is what registries are shared by.
func registryKey(src *registrySource) string {
	return src.describe() + "|" + src.interval.String() + "|" + src.prefix
}

func newRegistry(src *registrySource) *registry {
	r := &registry{source: src.describe(), prefix: src.prefix, now: time.Now}
	location := src.location
	if src.kind == sourceFile {
		r.load = func() ([]byte, error) { return readFileLimited(location) }
		return r
	}
	client := &http.Client{Timeout: 5 * time.Second}
	r.load = func() ([]byte, error) { return fetchLimited(client, location) }
	return r
}

func (r *registry) release() {
	registriesMu.Lock()
	defer registriesMu.Unlock()
	r.users--
	if r.users == 0 {
		r.idleSince = r.now()
	}
}

// retire removes the registry once nothing has used it for pollerIdleGrace.
func (r *registry) retire(key string) bool {
	registriesMu.Lock()
	defer registriesMu.Unlock()
	if r.users > 0 || r.now().Sub(r.idleSince) < pollerIdleGrace {
		return false
	}
	delete(registries, key)
	return true
}

// poll runs tick on every tick of the clock until it says it is done. The
// loop has no select: Yaegi v0.16.1 hangs on a labelled break out of one.
func (r *registry) poll(key string, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		if r.tick(key) {
			return
		}
	}
}

// tick is one turn of the poller: it refreshes the registry, or retires it
// and reports that the poller is done.
func (r *registry) tick(key string) (done bool) {
	if !r.retire(key) {
		r.refresh()
		return false
	}
	r.mu.Lock()
	r.retired = true
	r.mu.Unlock()
	infof("registry %s: no middleware reads it any more, polling stopped", r.source)
	return true
}

// current is the snapshot requests read now. A retired registry has no
// readers left, by Traefik's own account: each gave it up when its context
// ended, which Traefik does as it replaces the handler. One that is read
// all the same would serve, without a word, a snapshot that never changes
// again, so the first such read says so where an operator will see it.
func (r *registry) current() *snapshot {
	r.mu.RLock()
	snap, retired := r.snap, r.retired
	r.mu.RUnlock()
	if retired {
		r.frozen.Do(func() {
			warnf("registry %s: a middleware still reads it after its poller stopped, so it is served the snapshot it had and changes to the registry are not seen. Traefik ended the middleware's context without replacing it; restart Traefik to recover", r.source)
		})
	}
	return snap
}

// refresh loads, parses and validates the registry, swapping it in only on
// success. An error is logged once per distinct message rather than on every
// tick, and an unchanged document is not parsed again.
func (r *registry) refresh() {
	r.loadMu.Lock()
	defer r.loadMu.Unlock()

	data, err := r.load()
	if err == nil && r.lastData != nil && bytes.Equal(data, r.lastData) {
		r.recovered()
		return
	}
	var snap *snapshot
	var warnings []string
	if err == nil {
		snap, warnings, err = decodeRegistry(data, r.prefix)
	}
	if err != nil {
		r.failed(err)
		return
	}
	r.lastData = data
	r.recovered()

	r.mu.Lock()
	r.snap = snap
	r.mu.Unlock()
	infof("registry %s: loaded %d tenancies, %d accounts", r.source, len(snap.tenancies), len(snap.accounts))
	for _, warning := range warnings {
		warnf("registry %s: %s", r.source, warning)
	}
}

func (r *registry) failed(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	msg := err.Error()
	if msg == r.lastErr {
		return
	}
	r.lastErr = msg
	if r.snap == nil {
		warnf("registry %s: not loaded, so nothing routes from it: %v", r.source, err)
		return
	}
	warnf("registry %s: keeping the last good snapshot: %v", r.source, err)
}

func (r *registry) recovered() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastErr != "" {
		infof("registry %s: recovered", r.source)
	}
	r.lastErr = ""
}

// tenancy returns the unexpired entry for name, or why there is none.
func (r *registry) tenancy(name string) (*tenancy, string) {
	snap := r.current()
	if snap == nil {
		// The error itself is in the log, not in a response header.
		return nil, "registry not loaded"
	}
	t, ok := snap.tenancies[name]
	if !ok {
		return nil, fmt.Sprintf("tenancy %+q is not registered", name)
	}
	if !t.expiresAt.IsZero() && !r.now().Before(t.expiresAt) {
		return nil, fmt.Sprintf("tenancy %+q expired at %s", name, t.expiresAt.UTC().Format(time.RFC3339))
	}
	return t, ""
}

// account returns the tenancy a test account is bound to.
func (r *registry) account(user string) (string, bool) {
	snap := r.current()
	if snap == nil {
		return "", false
	}
	bound, ok := snap.accounts[user]
	return bound, ok
}

func readFileLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readLimited(f)
}

func fetchLimited(client *http.Client, endpoint string) ([]byte, error) {
	resp, err := client.Get(endpoint)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", endpoint, resp.Status)
	}
	return readLimited(resp.Body)
}

func readLimited(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxRegistryBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxRegistryBytes {
		return nil, fmt.Errorf("larger than %d bytes", maxRegistryBytes)
	}
	return data, nil
}
