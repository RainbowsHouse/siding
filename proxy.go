package siding

import (
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

// transport is shared by every SUT proxy so connections are pooled across
// requests and middleware instances. ReverseProxy itself is cheap to build,
// so one is created per request rather than cached per (client-influenced)
// target URL.
var transport = http.DefaultTransport.(*http.Transport).Clone()

// serveProxy forwards req to target. The original Host header is kept so the
// SUT sees the same virtual host the production backend would, and the
// Director form keeps Traefik's X-Forwarded-* headers and appends to
// X-Forwarded-For, as Traefik's own proxy does.
//
// A failing SUT is a 502, never a retry against production: the body may
// already be consumed, and quietly hitting prod would hide the test failure.
// failed, when not nil, is told of it.
func serveProxy(rw http.ResponseWriter, req *http.Request, target string, failed func(error)) {
	u, err := url.Parse(target)
	if err != nil {
		// Unreachable: targets are validated before they get here.
		http.Error(rw, "siding: invalid target", http.StatusBadGateway)
		return
	}
	proxy := &httputil.ReverseProxy{
		Director: func(out *http.Request) {
			out.URL.Scheme = u.Scheme
			out.URL.Host = u.Host
			out.URL.Path, out.URL.RawPath = joinURLPath(u, out.URL)
			if u.RawQuery != "" {
				if out.URL.RawQuery == "" {
					out.URL.RawQuery = u.RawQuery
				} else {
					out.URL.RawQuery = u.RawQuery + "&" + out.URL.RawQuery
				}
			}
		},
		Transport:     transport,
		FlushInterval: 100 * time.Millisecond,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if failed != nil {
				failed(err)
			}
			http.Error(w, "siding: service under test unavailable", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(rw, req)
}

const (
	// failureWindow is how long a target's failures go unreported after
	// one was.
	failureWindow = time.Minute
	// maxFailureTargets bounds how many targets are told apart. Targets
	// can come from the request, so past it the rest share one entry
	// rather than push the others out.
	maxFailureTargets = 64
	otherTargets      = "(other targets)"
)

// failureLog decides which failures of a SUT are worth a line: the first,
// and then one per failureWindow with a count of those in between. A client
// that keeps asking for a SUT that is down would otherwise write a line at
// ERROR for every request.
type failureLog struct {
	mu      sync.Mutex
	now     func() time.Time
	targets map[string]*failures
}

type failures struct {
	reported   time.Time
	unreported int
}

func newFailureLog() *failureLog {
	return &failureLog{now: time.Now, targets: map[string]*failures{}}
}

// report says whether a failure of target is to be logged, and how many
// went unreported since the last one that was.
func (l *failureLog) report(target string) (log bool, unreported int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	f, known := l.targets[target]
	if !known && len(l.targets) >= maxFailureTargets {
		l.forget(now)
	}
	if !known && len(l.targets) >= maxFailureTargets {
		target = otherTargets
		f, known = l.targets[target]
	}
	if !known {
		l.targets[target] = &failures{reported: now}
		return true, 0
	}
	if now.Sub(f.reported) < failureWindow {
		f.unreported++
		return false, 0
	}
	unreported = f.unreported
	f.reported, f.unreported = now, 0
	return true, unreported
}

// forget drops the targets that have not failed for a window, and have
// nothing left to report.
func (l *failureLog) forget(now time.Time) {
	for target, f := range l.targets {
		if f.unreported == 0 && now.Sub(f.reported) >= failureWindow {
			delete(l.targets, target)
		}
	}
}

// joinURLPath and singleJoiningSlash are net/http/httputil's, which keeps
// them unexported. Joining the escaped forms as well as the decoded ones is
// what lets a path such as /a%2Fb reach the SUT exactly as it reaches
// production.
func joinURLPath(a, b *url.URL) (path, rawpath string) {
	if a.RawPath == "" && b.RawPath == "" {
		return singleJoiningSlash(a.Path, b.Path), ""
	}
	// Same as singleJoiningSlash, but uses EscapedPath to determine whether
	// a slash should be added.
	apath := a.EscapedPath()
	bpath := b.EscapedPath()

	aslash := strings.HasSuffix(apath, "/")
	bslash := strings.HasPrefix(bpath, "/")

	if aslash && bslash {
		return a.Path + b.Path[1:], apath + bpath[1:]
	}
	if !aslash && !bslash {
		return a.Path + "/" + b.Path, apath + "/" + bpath
	}
	return a.Path + b.Path, apath + bpath
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	if aslash && bslash {
		return a + b[1:]
	}
	if !aslash && !bslash {
		return a + "/" + b
	}
	return a + b
}
