package siding

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const registryJSON = `{
  "tenancies": {
    "test/env-1": {"services": {"cats-openapi": "http://sut.svc:8080", "dogs": "http://dogs-sut.svc"}},
    "test/expired": {"services": {"cats-openapi": "http://old.svc"}, "expiresAt": "2020-01-01T00:00:00Z"}
  },
  "accounts": {"tester-1": "test/env-1"}
}`

func writeRegistry(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func fileSource(path string, interval time.Duration) *registrySource {
	return &registrySource{kind: sourceFile, location: path, interval: interval, prefix: "test/"}
}

// loadedRegistry is a registry of its own, loaded once and never polled.
func loadedRegistry(t *testing.T, src *registrySource) *registry {
	t.Helper()
	r := newRegistry(src)
	r.refresh()
	return r
}

// capture collects what a logger writes. It locks because pollers other
// tests left running log too.
type capture struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (c *capture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// captureLogs redirects both log sinks for the rest of the test.
func captureLogs(t *testing.T) (info, warn *capture) {
	t.Helper()
	info, warn = &capture{}, &capture{}
	sinkMu.Lock()
	oldInfo, oldWarn := infoSink, warnSink
	infoSink = func(line string) { _, _ = info.Write([]byte(line + "\n")) }
	warnSink = func(line string) { _, _ = warn.Write([]byte(line + "\n")) }
	sinkMu.Unlock()
	t.Cleanup(func() {
		sinkMu.Lock()
		infoSink, warnSink = oldInfo, oldWarn
		sinkMu.Unlock()
	})
	return info, warn
}

func TestRegistryFromFile(t *testing.T) {
	r := loadedRegistry(t, fileSource(writeRegistry(t, registryJSON), time.Hour))

	entry, reason := r.tenancy("test/env-1")
	if entry == nil {
		t.Fatalf("tenancy not found: %s", reason)
	}
	// Sorted by name, so injected baggage is stable.
	if len(entry.targets) != 2 || entry.targets[0].name != "cats-openapi" || entry.targets[1].name != "dogs" {
		t.Fatalf("got %+v", entry.targets)
	}
	if got := entry.targets[0]; got.url != "http://sut.svc:8080" || got.host != "sut.svc" || got.port != 8080 {
		t.Fatalf("got %+v", got)
	}
	// No port in the URL: the one its scheme implies.
	if got := entry.targets[1]; got.host != "dogs-sut.svc" || got.port != 80 {
		t.Fatalf("got %+v", got)
	}
	if tenancy, ok := r.account("tester-1"); !ok || tenancy != "test/env-1" {
		t.Fatalf("account = %q, %v", tenancy, ok)
	}
	if _, ok := r.account("nobody"); ok {
		t.Fatal("unknown account resolved")
	}
}

func TestRegistryLookupSaysWhy(t *testing.T) {
	r := loadedRegistry(t, fileSource(writeRegistry(t, registryJSON), time.Hour))
	for name, want := range map[string]string{
		"test/expired": `tenancy "test/expired" expired at 2020-01-01T00:00:00Z`,
		"test/unknown": `tenancy "test/unknown" is not registered`,
	} {
		if entry, reason := r.tenancy(name); entry != nil || reason != want {
			t.Errorf("%s: got %v, %q; want %q", name, entry, reason, want)
		}
	}
	r.now = func() time.Time { return time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC) }
	if entry, reason := r.tenancy("test/expired"); entry == nil {
		t.Fatalf("tenancy not yet expired did not resolve: %s", reason)
	}
}

func TestDecodeRegistryIsStrict(t *testing.T) {
	for doc, want := range map[string]string{
		`{not json`:        "not a registry document",
		`{"tenancys": {}}`: `unknown field "tenancys"`,
		`{"tenancies": {"test/x": {"service": {"a": "http://a.svc"}}}}`: `unknown field "service"`,
		`{"tenancies": {}} {}`: "data after the closing brace",
		`{"tenancies": {"": {"services": {"a": "http://a.svc"}}}}`:              "empty name",
		`{"tenancies": {"test/x": {"services": {"BAD NAME": "http://a.svc"}}}}`: "invalid service name",
		`{"tenancies": {"test/x": {"services": {"a": "ftp://a.svc"}}}}`:         `invalid URL for service "a"`,
		`{"tenancies": {"test/x": {"expiresAt": "tomorrow"}}}`:                  "not a registry document",
		`{"accounts": {"tester-1": ""}}`:                                        "neither may be empty",
	} {
		_, _, err := decodeRegistry([]byte(doc), "test/")
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", doc, err, want)
		}
	}
}

func TestDecodeRegistryWarnings(t *testing.T) {
	snap, warnings, err := decodeRegistry([]byte(`{
	  "tenancies": {"test/empty": {}, "test/ok": {"services": {"a": "http://a.svc"}}},
	  "accounts": {"lost": "test/gone", "found": "test/ok"}
	}`), "test/")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.tenancies) != 2 || len(snap.accounts) != 2 {
		t.Fatalf("got %d tenancies, %d accounts", len(snap.tenancies), len(snap.accounts))
	}
	want := []string{
		`tenancy "test/empty" has no services`,
		`account "lost" is bound to tenancy "test/gone", which the registry does not define`,
	}
	if strings.Join(warnings, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q", warnings)
	}
	if _, warnings, _ := decodeRegistry([]byte(registryJSON), "test/"); len(warnings) != 0 {
		t.Fatalf("clean registry warned: %q", warnings)
	}
}

// A tenancy outside the reader's tenancyPrefix is never routed, so an entry
// for one, or an account bound to one, is there by mistake.
func TestDecodeRegistryWarnsOfEntriesOutsideThePrefix(t *testing.T) {
	services := `{"services": {"a": "http://a.svc"}}`
	_, warnings, err := decodeRegistry([]byte(`{
	  "tenancies": {
	    "test/ok": `+services+`, "testing": `+services+`,
	    "p1": `+services+`, "p2": `+services+`, "p3": `+services+`,
	    "p4": `+services+`, "p5": `+services+`, "p6": `+services+`},
	  "accounts": {"tester": "test/ok", "prod-user": "prod", "other": "p1"}
	}`), "test/")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		`tenancies outside tenancyPrefix "test/" are never routed: "p1", "p2", "p3", "p4", "p5" and 2 more`,
		`accounts bound to a tenancy outside tenancyPrefix "test/" are never routed: "other", "prod-user"`,
	}
	if strings.Join(warnings, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got %q", warnings)
	}
}

func TestRegistryKeepsLastGoodSnapshot(t *testing.T) {
	_, warn := captureLogs(t)
	path := writeRegistry(t, registryJSON)
	r := loadedRegistry(t, fileSource(path, time.Hour))

	for _, bad := range []string{
		"{not json",
		`{"tenancys": {}}`,
		`{"tenancies": {"test/x": {"services": {"BAD NAME": "http://a.svc"}}}}`,
		`{"tenancies": {"test/x": {"services": {"a": "ftp://a.svc"}}}}`,
	} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		r.refresh()
		if entry, _ := r.tenancy("test/env-1"); entry == nil {
			t.Fatalf("bad reload %q dropped the good snapshot", bad)
		}
	}
	// A failure is for the operator to see, so it goes to the stderr logger.
	if got := warn.String(); !strings.Contains(got, "keeping the last good snapshot") || !strings.Contains(got, `unknown field "tenancys"`) {
		t.Fatalf("load failures not logged: %q", got)
	}

	if err := os.WriteFile(path, []byte(`{"tenancies": {"test/new": {"services": {"a": "http://a.svc"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	r.refresh()
	if entry, _ := r.tenancy("test/new"); entry == nil {
		t.Fatal("good reload not applied")
	}
	if entry, _ := r.tenancy("test/env-1"); entry != nil {
		t.Fatal("removed tenancy still resolves")
	}
}

func TestRegistryLogsAFailureOnce(t *testing.T) {
	info, warn := captureLogs(t)
	path := writeRegistry(t, "{not json")
	r := loadedRegistry(t, fileSource(path, time.Hour))
	r.refresh()
	r.refresh()
	if got := strings.Count(warn.String(), "not loaded, so nothing routes from it"); got != 1 {
		t.Fatalf("logged %d times: %q", got, warn.String())
	}

	if err := os.WriteFile(path, []byte(registryJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	r.refresh()
	r.refresh() // unchanged: not parsed, and not announced, again
	got := info.String()
	if strings.Count(got, "recovered") != 1 || strings.Count(got, "loaded 2 tenancies, 1 accounts") != 1 {
		t.Fatalf("got %q", got)
	}
}

func TestRegistryMissingFileStartsEmpty(t *testing.T) {
	r := loadedRegistry(t, fileSource(filepath.Join(t.TempDir(), "missing.json"), time.Hour))
	if entry, reason := r.tenancy("test/env-1"); entry != nil || reason != "registry not loaded" {
		t.Fatalf("got %v, %q", entry, reason)
	}
	if _, ok := r.account("tester-1"); ok {
		t.Fatal("account resolved from an empty registry")
	}
}

func TestRegistryFromURL(t *testing.T) {
	var mu sync.Mutex
	status, body := http.StatusOK, registryJSON
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	r := loadedRegistry(t, &registrySource{kind: sourceURL, location: srv.URL, interval: time.Hour, prefix: "test/"})
	if entry, reason := r.tenancy("test/env-1"); entry == nil {
		t.Fatalf("tenancy not loaded over HTTP: %s", reason)
	}

	mu.Lock()
	status, body = http.StatusInternalServerError, "{}"
	mu.Unlock()
	r.refresh()
	if entry, _ := r.tenancy("test/env-1"); entry == nil {
		t.Fatal("HTTP error dropped the good snapshot")
	}
}

func TestSharedRegistryIsReused(t *testing.T) {
	path := writeRegistry(t, registryJSON)
	a := openRegistry(context.Background(), fileSource(path, time.Hour))
	b := openRegistry(context.Background(), fileSource(path, time.Hour))
	c := openRegistry(context.Background(), fileSource(path, 2*time.Hour))
	if a != b {
		t.Fatal("same source and interval started a second poller")
	}
	if a == c {
		t.Fatal("a different interval reused the poller")
	}
	// A registry checks its entries against one tenancyPrefix.
	other := fileSource(path, time.Hour)
	other.prefix = "e2e/"
	if d := openRegistry(context.Background(), other); a == d {
		t.Fatal("a different tenancyPrefix reused the registry")
	}
}

// The one test that waits on the clock: the poller itself.
func TestRegistryPollPicksUpChanges(t *testing.T) {
	path := writeRegistry(t, `{}`)
	r := openRegistry(context.Background(), fileSource(path, 20*time.Millisecond))
	if err := os.WriteFile(path, []byte(registryJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if !eventually(func() bool {
		entry, _ := r.tenancy("test/env-1")
		return entry != nil
	}) {
		t.Fatal("poller never picked up the new registry")
	}
}

func registered(r *registry) bool {
	registriesMu.Lock()
	defer registriesMu.Unlock()
	for _, other := range registries {
		if other == r {
			return true
		}
	}
	return false
}

func users(r *registry) int {
	registriesMu.Lock()
	defer registriesMu.Unlock()
	return r.users
}

// eventually waits for something another goroutine does. The deadline is
// only reached when it never happens.
func eventually(condition func() bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// clock is a time the test moves by hand.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// openWithClock opens a registry whose poller never ticks by itself, on a
// clock of the test's: the test turns the poller with tick.
func openWithClock(t *testing.T, ctx context.Context, src *registrySource) (*registry, *clock) {
	t.Helper()
	c := &clock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	r := openRegistry(ctx, src)
	registriesMu.Lock()
	r.now = func() time.Time { return c.read() }
	registriesMu.Unlock()
	return r, c
}

// Traefik cancels the old configuration's context before it builds the new
// handlers, so for a moment a registry has no users.
func TestRegistrySurvivesAReload(t *testing.T) {
	src := fileSource(writeRegistry(t, registryJSON), time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	first, clock := openWithClock(t, ctx, src)

	cancel()
	if !eventually(func() bool { return users(first) == 0 }) {
		t.Fatal("cancelling the context did not release the registry")
	}
	clock.advance(pollerIdleGrace - time.Second)
	if first.tick(registryKey(src)) {
		t.Fatal("retired within the grace")
	}

	if second := openRegistry(context.Background(), src); second != first {
		t.Fatal("the rebuild got a new registry, and lost the snapshot")
	}
	clock.advance(time.Hour)
	if first.tick(registryKey(src)) || users(first) != 1 {
		t.Fatalf("retired while held; users = %d", users(first))
	}
}

func TestRegistryRetiresWhenUnused(t *testing.T) {
	info, warn := captureLogs(t)
	src := fileSource(writeRegistry(t, registryJSON), time.Hour)
	key := registryKey(src)
	r, clock := openWithClock(t, context.Background(), src)
	openRegistry(context.Background(), src)

	r.release()
	clock.advance(time.Hour)
	if r.tick(key) || !registered(r) {
		t.Fatal("retired while a middleware still held it")
	}
	r.release()
	if r.tick(key) {
		t.Fatal("retired the moment it was released")
	}
	clock.advance(pollerIdleGrace)
	if !r.tick(key) || registered(r) {
		t.Fatal("not retired after the grace")
	}
	if !strings.Contains(info.String(), "polling stopped") {
		t.Fatalf("not logged: %q", info.String())
	}
	if warn.String() != "" {
		t.Fatalf("a registry nobody reads warned: %q", warn.String())
	}
	if again := openRegistry(context.Background(), src); again == r {
		t.Fatal("a retired registry was handed out again")
	}
}

// Nothing should read a retired registry. If something does, it is served
// what the registry had, and told once, where an operator will see it.
func TestRetiredRegistryStillReadSaysSo(t *testing.T) {
	_, warn := captureLogs(t)
	src := fileSource(writeRegistry(t, registryJSON), time.Hour)
	r, clock := openWithClock(t, context.Background(), src)
	r.release()
	clock.advance(pollerIdleGrace)
	if !r.tick(registryKey(src)) {
		t.Fatal("not retired")
	}

	for i := 0; i < 3; i++ {
		if entry, reason := r.tenancy("test/env-1"); entry == nil {
			t.Fatalf("the snapshot was dropped: %s", reason)
		}
		if bound, _ := r.account("tester-1"); bound != "test/env-1" {
			t.Fatalf("account = %q", bound)
		}
	}
	if got := strings.Count(warn.String(), "still reads it after its poller stopped"); got != 1 {
		t.Fatalf("logged %d times: %q", got, warn.String())
	}
}
