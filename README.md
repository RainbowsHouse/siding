# siding

Request-level test routing for [Traefik](https://traefik.io/traefik/).

A siding is the track beside the main line that a train is switched onto.
Here the train is a test request: it travels the production route like any
other, and is switched off it only at the services that have a version under
test. Everything else it touches is production, and every other request never
notices.

siding is a Traefik middleware plugin. A request names a **tenancy** in its
[W3C baggage](https://www.w3.org/TR/baggage/), and the middleware in front of
a service sends the request to the **service under test (SUT)** registered
for that tenancy, if there is one:

```sh
curl https://cats.example.com/                                           # production
curl -H 'baggage: request-tenancy=test/pr-123' https://cats.example.com/ # the SUT of pr-123
```

The approach is that of Uber's
[SLATE](https://www.uber.com/blog/simplifying-developer-testing-through-slate/),
which this project is inspired by. It is not affiliated with Uber, and shares
no code with SLATE, which is not open source.

| SLATE                                     | siding                                                          |
| ----------------------------------------- | --------------------------------------------------------------- |
| Jaeger baggage `request-tenancy`          | W3C `baggage` member `request-tenancy=test/…`                   |
| config-cache (tenancy → SUT routing info) | `registry`: a file, a URL, or the middleware's own config       |
| TestAccounts bound to an environment      | registry `accounts`, looked up via `userHeader`                 |
| Edge-Gateway injects `routing-overrides`  | `injectBaggage`: resolved routing written back into the baggage |
| Edge-Gateway decides who is a test client | `trustedSources`: whose routing baggage is accepted             |
| host-routing-agent                        | the middleware itself: reverse-proxies to the SUT               |
| fallback to production                    | no mapping → the router's normal service                        |

## Status

- **Traefik v3.3 to v3.7.** Every release in that range interprets plugins
  with the same Yaegi, v0.16.1. CI runs the demo against each.
- **Pre-1.0.** Configuration keys may still change between minor versions.
- **`v0.1.1` is the first version the catalog can install.** `v0.1.0` spelt
  the module path `github.com/rainbowshouse/siding`, which the catalog
  refuses: the repository is `RainbowsHouse/siding`. Loaded locally, both
  work.
- **Kubernetes discovery is a design, not yet code:** see
  [docs/design/discovery.md](docs/design/discovery.md).

## Install

Traefik loads a plugin from its static configuration, in one of two ways.

### From the plugin catalog

```yaml
# Traefik static configuration
experimental:
  plugins:
    siding:
      moduleName: github.com/RainbowsHouse/siding
      version: v0.1.1
```

Traefik downloads the plugin from its catalog **every time it starts**. If
that fails, it starts with all plugins disabled and drops every router that
uses one. Where that matters, load it locally.

### Locally, from a ConfigMap

Each release carries `siding-plugin-X.Y.Z.yaml`: a ConfigMap of that name
holding the plugin's source. Nothing is downloaded when Traefik starts.

```sh
kubectl apply -n traefik -f https://github.com/RainbowsHouse/siding/releases/download/v0.1.1/siding-plugin-0.1.1.yaml
```

```yaml
# values of the traefik/traefik Helm chart
additionalArguments:
  - '--experimental.localPlugins.siding.moduleName=github.com/RainbowsHouse/siding'
deployment:
  additionalVolumes:
    - name: siding-plugin
      configMap:
        name: siding-plugin-0.1.1
        optional: true
additionalVolumeMounts:
  - name: siding-plugin
    mountPath: /plugins-local/src/github.com/RainbowsHouse/siding
```

- **Upgrading is a change to that ConfigMap's name.** Traefik reads a
  plugin's source once, when it starts. A new name changes the pod template,
  so the replicas roll onto the new code together.
- **`optional: true`** lets a pod start when the ConfigMap is missing. The
  plugin then fails to load and its routers are dropped, which costs less
  than pods that cannot start at all.

### The Helm chart

The chart renders one `Middleware` per service you guard. It does not load
the plugin into Traefik: that is one of the two ways above.

```sh
helm repo add siding https://rainbowshouse.github.io/siding
helm install siding siding/siding -n traefik -f values.yaml
```

```yaml
# values.yaml
defaults:
  registry:
    url: http://registry.example.svc:8080/registry
services:
  cats-openapi: {}
  dogs-api:
    userHeader: Remote-User
```

This renders the Middlewares `siding-cats-openapi` and `siding-dogs-api`.
Attach one to a route where you would attach any Traefik middleware, such as
an `ExtensionRef` filter on an HTTPRoute. See
[charts/siding](charts/siding/README.md).

## Request flow

1. Parse the `baggage` header. Anything over the W3C limits (8192 bytes or 180
   members) falls back. It is forwarded untouched, unless it names the
   tenancy or overrides key: nothing in it can be checked, so it is dropped
   whole rather than let padding carry a forged member to the next hop.
2. With `trustedSources`, a request from any other peer has its tenancy and
   overrides members removed before anything reads them.
3. **Tenancy**: the `tenancyKey` member if present, else the registry account
   for the `userHeader` value. A tenancy outside `tenancyPrefix` is never
   routed, whatever else the request carries.
4. **Overrides**: an explicit `overridesKey` member when `allowHeaderOverrides`
   is on, otherwise the tenancy's registry entry.
   - The member is base64 JSON, `{"services":[{"name":"…","url":"…"}]}`, in
     any base64 flavour. What the middleware writes is unpadded and URL-safe.
   - A rejected member is **stripped** from the baggage, so a downstream hop
     that does trust header overrides never sees a forged one. So is a second
     member with the same key, one whose value doesn't percent-decode, and
     one whose key differs only in case (`Routing-Overrides`): the two
     routing keys are this middleware's, however they are spelt.
5. With `injectBaggage`, write the tenancy and overrides back into the
   baggage. Later hops that come back through Traefik then route without a
   lookup, provided services propagate baggage (OpenTelemetry does).
   - Nothing is written that would take the baggage past the W3C limits: the
     next hop would refuse all of it. Short of room, the overrides are left
     out first, then the tenancy.
6. If the overrides name this middleware's `service`, reverse-proxy to that
   URL:
   - The original `Host` is kept.
   - Traefik's `X-Forwarded-*` headers are kept.
   - The path is kept as it was sent, escapes included (`/a%2Fb`).
   - A failing SUT is a **502, not a retry against production**, so a broken
     SUT can't be hidden by production answering instead.

   Otherwise call the normal backend.

## Configuration

The least that routes anything: the service's name, and a registry.

```yaml
http:
  middlewares:
    siding-cats-openapi:
      plugin:
        siding:
          service: cats-openapi
          registry:
            file: /etc/siding/registry.json
```

Every key:

| Key                        | Default             | Meaning                                                                                                            |
| -------------------------- | ------------------- | ------------------------------------------------------------------------------------------------------------------ |
| `service`                  | none, **required**  | The name this middleware's backend has in the registry and in overrides. `[a-z0-9-]`, at most 32 characters.       |
| `registry.file`            | none                | Path of a [registry document](#registry), re-read every `refreshInterval`.                                         |
| `registry.url`             | none                | `http(s)` URL of one, fetched every `refreshInterval` with a 5s timeout.                                           |
| `registry.tenancies`       | none                | The registry's `tenancies`, [written inline](#inline-registry).                                                    |
| `registry.accounts`        | none                | The registry's `accounts`, written inline.                                                                         |
| `registry.refreshInterval` | `10s`               | A Go duration (`30s`, `1m`) or a number of seconds. At least `1s`.                                                 |
| `userHeader`               | none                | Request header holding the account to look up in the registry's `accounts`. Needs a registry.                      |
| `tenancyPrefix`            | `test/`             | Only tenancies starting with it are routed. May not be empty.                                                      |
| `allowHeaderOverrides`     | `false`             | Accept a routing-overrides member sent with the request. Needs `allowedHosts`.                                     |
| `allowedHosts`             | none                | Endpoints a SUT URL may name. See [`allowedHosts`](#allowedhosts).                                                 |
| `trustedSources`           | none: every peer    | CIDRs or addresses whose routing baggage is accepted. See [Trust model](#trust-model).                             |
| `injectBaggage`            | `true`              | Write resolved routing back into the baggage.                                                                      |
| `debugHeader`              | none: off           | Response header that reports `routed=<url>` or `fallback=<reason>`. Never sent to a peer outside `trustedSources`. |
| `baggageHeader`            | `baggage`           | The header carrying W3C baggage.                                                                                   |
| `tenancyKey`               | `request-tenancy`   | Baggage key of the tenancy.                                                                                        |
| `overridesKey`             | `routing-overrides` | Baggage key of the overrides payload. Must differ from `tenancyKey`.                                               |

- **`registry`** takes one source: `file`, `url`, or inline
  `tenancies`/`accounts`. With none, only header overrides can route.
- **`debugHeader`** names internal hosts, so it is off unless set. Plain
  traffic with no tenancy and no overrides gets no header either way. Its
  value is ASCII and at most 256 bytes, whatever the request carried.
- **Header names** (`baggageHeader`, `userHeader`, `debugHeader`) must differ
  from one another, and none may be a header with a meaning of its own, such
  as `Host`, `Content-Length`, `Authorization` or `Set-Cookie`.

### `allowedHosts`

Each entry is a host, or a leading dot for every host under a domain, with an
optional port:

```yaml
allowedHosts:
  - 'cats.siding-sut.svc:8080' # this host, this port
  - '.siding-sut.svc:8080' # every host under siding-sut.svc, this port
  - '[::1]:8080' # IPv6 in brackets
  - '.siding-sut.svc' # any port: not with allowHeaderOverrides
```

- **Quote the entries.** An unquoted `[::1]:8080` is a YAML list, and Traefik
  then fails to read the whole file.
- **With `allowHeaderOverrides`, every entry names its port**, and `New`
  refuses one that doesn't. Clients supply the target, so an entry without a
  port would let them reach every port on the host. One target outside the
  list rejects the whole payload.
- **Without it, the port is optional.** The list then bounds only registry
  targets, which the operator wrote. A target outside it is ignored (and
  logged once), and the tenancy's other services still route. With no list at
  all, registry targets are unrestricted.
- **The port compared is the one that would be dialled.** `http://sut` is
  port 80 and `https://sut` is 443. The scheme itself is not part of an
  entry: `sut:8080` admits both.
- **List SUTs, not the cluster.** An entry such as `.default.svc` admits
  everything in that namespace, the services you would least want a request
  steered to among them. Give SUTs a namespace of their own and list that.

### What a mistake does

A value the middleware can't use is an error from `New`, which names the key
and the value:

```text
allowedHosts[0] "*.svc": wildcards are not supported; use a host ("cats.siding-sut.svc:8080") or a leading dot for every host under a domain (".siding-sut.svc:8080")
registry.refreshInterval "500ms": must be at least 1s
nothing can route: set registry.file, registry.url or registry.tenancies, or allowHeaderOverrides with allowedHosts
```

Traefik logs the error and **drops every router that uses the middleware
until the configuration is fixed**. Their requests get a 404, production
traffic included, unless another router matches them. That is deliberate: the
alternative is a middleware that quietly does nothing.

Some things are out of the plugin's hands:

- **A misspelt key is dropped by Traefik** before the plugin sees it.
  `registery:` is caught, because nothing can then route. `trustedSource:` is
  not: the middleware loads, trusting every peer. Check the line the plugin
  logs at start-up (below).
- **An empty `registry: {}`** is rejected by Traefik's own decoder
  (`'Registry' expected a map, got 'string'`). Leave the key out instead.
- **An unquoted `expiresAt`** in an inline registry is read by YAML as a
  timestamp, and Traefik's file provider then refuses the **whole dynamic
  file**, every router in it included
  (`field expiresAt uses unsupported type: struct`). Write
  `expiresAt: '2026-10-01T00:00:00Z'`, in quotes. So with an unquoted
  `[::1]:8080` in `allowedHosts`.

A registry that is missing or invalid **at run time** is not an error from
`New`: the middleware starts, routes nothing from it, and says so in the log.
Production traffic is unaffected.

### Logs

The plugin logs through Traefik's own logger, so its lines carry Traefik's
timestamps, format and level filter:

| Level   | What                                                                                                                                                             |
| ------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ERROR` | A registry that failed to load (once per distinct error), registry entries that are ignored, a failing SUT (once a minute per target, with a count of the rest). |
| `DEBUG` | The effective configuration of each middleware, registry loads and recoveries, stripped baggage.                                                                 |

```text
ERR [siding] registry file:/etc/siding/registry.json: keeping the last good snapshot: not a registry document: json: unknown field "tenancys"
DBG [siding] siding-cats-openapi@file: service=cats-openapi registry=file:/etc/siding/registry.json headerOverrides=false allowedHosts=0 trustedSources=1 injectBaggage=true
```

There is no level in between: Traefik gives a plugin two writers, one at
`DEBUG` and one at `ERROR`.

### Registry

```json
{
  "tenancies": {
    "test/pr-123": {
      "services": { "cats-openapi": "http://cats-openapi.siding-sut.svc:8080" },
      "expiresAt": "2026-10-01T00:00:00Z"
    }
  },
  "accounts": { "tester-1": "test/pr-123" }
}
```

- **Strict:** an unknown member (`"tenancys"`, `"service"`) fails the load
  rather than loading as an empty registry. So does any invalid service name
  or URL, and the whole document is then refused.
- **Refresh:** re-read every `refreshInterval`. A load that fails to read,
  parse or validate keeps the last good snapshot, and the error is logged
  once.
- **Expiry:** `expiresAt` is optional; once it passes, the tenancy stops
  resolving.
- **Warnings:** entries that can do nothing are logged and otherwise
  harmless: a tenancy with no services, an account bound to a tenancy the
  document doesn't define, and a tenancy or an account's binding outside
  `tenancyPrefix`.
- **One poller per source:** every middleware instance with the same source,
  interval and `tenancyPrefix` shares one poller, which outlives Traefik's
  configuration reloads and stops 30s after the last middleware reading it
  is gone.
  - This leans on Traefik ending a middleware's context when it replaces the
    middleware. If a registry is ever read after its poller stopped, the
    first such read logs it at `ERROR`: the snapshot is still served, but no
    longer changes.
- **A slow URL delays reloads:** the first fetch happens while Traefik builds
  its configuration, so a registry URL that doesn't answer holds a reload up
  for the 5s timeout.

### Inline registry

The same two members, in the middleware's configuration. Traefik rebuilds the
middleware when the configuration changes, so there is nothing to poll and,
in Kubernetes, no file to mount into the Traefik pods:

```yaml
apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: siding-cats-openapi
spec:
  plugin:
    siding:
      service: cats-openapi
      registry:
        tenancies:
          test/pr-123:
            services:
              cats-openapi: http://cats-openapi.siding-sut.svc:8080
            expiresAt: '2026-10-01T00:00:00Z'
        accounts:
          tester-1: test/pr-123
```

- It is validated like a registry document, except that what a polled
  registry only warns about is an error here.
- Quote `expiresAt`. Unquoted, it costs the file provider the whole file, not
  this middleware alone: see [What a mistake does](#what-a-mistake-does).
- Each middleware carries its own copy. With several services under test, a
  shared `file` or `url` keeps them in one place.
- It works with the file and Kubernetes CRD providers. Label and KV providers
  split keys on `.`, which tenancy names and hosts contain.

## Trust model

Routing sends a request to a build that hasn't been released. Who can ask for
that is decided by three settings:

- **The tenancy in the baggage is believed as sent.** Anyone who can reach
  the router can send `baggage: request-tenancy=test/…`. `tenancyPrefix`
  limits which tenancies route, not who may name one.
- **`trustedSources` limits who is believed.** A request from any other peer
  has its tenancy and overrides members removed, so it neither routes here
  nor carries them to a later hop, and gets no debug header. The rest of its
  baggage is left alone.
  - The peer is the connection's own address. `X-Forwarded-For` is never
    consulted: the client writes it.
  - List the networks that _later hops_ come from, such as the pod CIDR, and
    leave the clients out.
  - **It only works where Traefik sees the client's address.** Behind a load
    balancer that rewrites the source, as k3s's ServiceLB does unless the
    Service has `externalTrafficPolicy: Local`, every client arrives from a
    cluster address and is trusted along with the pods. Look at the client
    address in Traefik's access log for a request from outside before relying
    on it.
- **`userHeader` is the way in for everyone else.** An account bound to a
  tenancy in the registry routes from any peer. The header has to come from
  something that authenticated the user, such as a forwardAuth middleware
  placed **before** this one that overwrites whatever the client sent. On a
  router without one, the client chooses its own account.

`allowHeaderOverrides` lets the request name the target itself, bounded by
`allowedHosts`, host and port. It needs no tenancy. Leave it off at the edge.

## Why a Go plugin

Traefik picks a router's backend before its middlewares run, so sending a
request somewhere else means proxying it from inside the middleware. A
WebAssembly plugin can't make outbound requests. A Go plugin, which Traefik
interprets with Yaegi, can.

## Development

```sh
gofmt -l . && go vet ./... && go test -race ./...
go run github.com/traefik/yaegi/cmd/yaegi@v0.16.1 test .   # the same tests, interpreted
```

Traefik interprets the plugin with **Yaegi v0.16.1**, so:

- Keep the code at the Go 1.22 language level, in one package, and use only
  the standard library.
- The second command exists because the interpreter has quirks that `go test`
  can't see:
  - It only evaluates the first expression of a tagless `case a, b:` list.
  - It mistypes method values such as `srv.Close`; wrap them in a closure.
  - It hangs on a labelled `break` out of a `select`. `return` from the
    `select`, or a loop without one, works.
  - It marshals a struct's unexported fields, as `X<name>`. Keep derived data
    out of the structs that are encoded as JSON.
  - It panics on a tuple assignment with `nil` in it (`a, b = nil, ""`).
    Assign one at a time.
- Log with `fmt.Print*` (Traefik's `DEBUG`) and the `log` package's own
  functions (`ERROR`). Yaegi redirects only those: `os.Stdout` and `os.Stderr`
  stay the process's streams, so a `log.New(os.Stderr, …)` writes raw lines
  past Traefik's logger, whatever the log level.

### Demo

```sh
cd demo
docker compose up -d   # Traefik + prod (go-httpbin) + sut (whoami)
./demo-test.sh
docker compose down
```

`dynamic.yaml` puts the middleware (as `cats-openapi`) in front of `prod`
three times, and `registry.json` maps `test/env-1` to `sut`:

| Host               | Configuration                                                       |
| ------------------ | ------------------------------------------------------------------- |
| any                | file registry, header overrides allowed for `sut:80`                |
| `inline.localhost` | inline registry                                                     |
| `edge.localhost`   | file registry, `trustedSources` that no peer in the demo belongs to |

```sh
curl -i localhost:8000/get                                        # prod
curl -i -H 'baggage: request-tenancy=test/env-1' localhost:8000/  # sut
curl -i -H 'X-User-Id: tester-1' localhost:8000/                  # sut, via the account

# The same tenancy from a peer that isn't trusted goes to prod
curl -i -H 'Host: edge.localhost' -H 'baggage: request-tenancy=test/env-1' localhost:8000/get
```

| Variable          | Default         | Meaning                                |
| ----------------- | --------------- | -------------------------------------- |
| `TRAEFIK_VERSION` | `v3.5`          | The Traefik image tag                  |
| `DEMO_PORT`       | `8000`          | The port published on `127.0.0.1`      |
| `PLUGIN_DIR`      | the repo's root | Where the plugin's source is read from |

`registry.json` is re-read every 2s, so you can edit it while the demo runs.
The plugin's `DEBUG` lines need `--log.level=DEBUG` in `docker-compose.yaml`.
After changing the plugin's source, restart Traefik: it reads the source once.

### Releases

One version for everything in the repository: a `vX.Y.Z` tag releases the
plugin, the chart and the plugin's ConfigMap together. **No other tags.**
Traefik's catalog refuses a repository that has a tag which isn't a plain
version, and CI fails when one appears.

## Not done yet

- **Discovery:** a controller that builds the registry from Kubernetes, so
  SUTs register by being deployed. Designed in
  [docs/design/discovery.md](docs/design/discovery.md).
- **Async flows:** baggage carried through queue payloads.
- **Per-protocol SUT ports:** SLATE tracks http, http2 and tchannel ports
  separately.

## Licence

[Apache-2.0](LICENSE). [NOTICE](NOTICE) lists what is included from others.
