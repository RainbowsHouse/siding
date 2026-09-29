# Design: discovering services under test from Kubernetes

**Status: proposal. None of this is built.** It is here to be argued with.

## The problem

siding routes by a registry: for each tenancy, which URL stands in for which
service. Today someone writes that registry, as a file, behind a URL, or into
a Middleware. Every service under test (SUT) that comes or goes is an edit to
it, made by whoever deploys the SUT or by a script of theirs.

In Kubernetes the facts the registry states are already in the cluster. A SUT
is a Service. Deploying it should be all it takes to register it, and
deleting it all it takes to remove it.

## Proposal in one paragraph

A small controller watches Services that opt in with a label, reads what they
stand in for from their annotations, and serves the result as a registry
document over HTTP. The plugin does not change: it polls that URL through
`registry.url`, as it polls any other. A `Tenancy` resource comes later, for
what annotations express badly.

```text
  Service (labelled, annotated)          Middleware (one per guarded service)
            │ watch                                 │ registry.url
            ▼                                       ▼
   ┌──────────────────┐    GET /registry    ┌────────────────┐
   │ siding controller│ ◀────────────────── │ plugin, inside │ ──▶ SUT or production
   └──────────────────┘   every few seconds │    Traefik     │
                                            └────────────────┘
```

## Registering a SUT

```yaml
apiVersion: v1
kind: Service
metadata:
  name: cats-openapi-pr-123
  namespace: siding-sut
  labels:
    siding.rainbows.house/sut: 'true'
  annotations:
    siding.rainbows.house/tenancy: test/pr-123
    siding.rainbows.house/service: cats-openapi
spec:
  selector: { app: cats-openapi, pr: '123' }
  ports:
    - name: http
      port: 8080
```

That Service becomes this registry entry:

```json
{
  "tenancies": {
    "test/pr-123": {
      "services": {
        "cats-openapi": "http://cats-openapi-pr-123.siding-sut.svc:8080"
      }
    }
  }
}
```

### The label and the annotations

| Key                             | Required | Meaning                                                                                            |
| ------------------------------- | -------- | -------------------------------------------------------------------------------------------------- |
| `siding.rainbows.house/sut`     | yes      | A **label**, `'true'`. The controller watches only Services that carry it.                         |
| `siding.rainbows.house/tenancy` | yes      | The tenancy the SUT belongs to.                                                                    |
| `siding.rainbows.house/service` | yes      | The service it stands in for, by the name that service's Middleware has as `service`.              |
| `siding.rainbows.house/port`    | no       | A port of the Service, by name or number. Default: its only port, or the one named `http`.         |
| `siding.rainbows.house/scheme`  | no       | `http` (default) or `https`.                                                                       |
| `siding.rainbows.house/ttl`     | no       | How long the entry lives, from the Service's `creationTimestamp`: `24h`.                           |
| `siding.rainbows.house/class`   | no       | Which controller this is for, where several run. A controller ignores a class that is not its own. |

- **The opt-in is a label** because a watch can select on labels. The
  controller then never sees, and needs no permission to read beyond, the
  Services that ask for it.
- **The rest are annotations** because a tenancy contains `/`, which a label
  value may not.
- **The URL has one form:** `<scheme>://<name>.<namespace>.svc:<port>`. No
  cluster domain, no path. One form is what lets an allowlist be written
  once and stay true.

## The controller

- **Compiled Go, not a plugin.** It is a Deployment of its own, released as a
  container image with the plugin's version.
- **Read-only.** It needs `get`, `list` and `watch` on Services, and `create`
  on Events. It writes nothing else: no annotations, no status, no
  Middlewares.
- **Stateless.** Every replica builds the same document from its own cache,
  so there is no leader to elect. Two replicas can disagree for the moment
  between one seeing a change and the other.
- **Endpoints:**

  | Path        | Answers                                                          |
  | ----------- | ---------------------------------------------------------------- |
  | `/registry` | The registry document; 503 until its cache has synced            |
  | `/healthz`  | Liveness                                                         |
  | `/readyz`   | Ready once the cache has synced                                  |
  | `/metrics`  | Entries served, entries refused and why, time of the last change |

How long a SUT takes to start receiving requests: the controller's watch
delay, plus up to one `refreshInterval` of the plugin.

## Decisions, and why

| Question                                                  | Answer                                                                                       | Why                                                                                                                                                  |
| --------------------------------------------------------- | -------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| What does `/registry` answer before the cache has synced? | 503                                                                                          | An empty document would be a valid registry with nothing in it, and send test requests to production. On a 503 the plugin keeps the snapshot it has. |
| One invalid entry                                         | Left out, with an Event on its Service                                                       | The plugin refuses a whole document for one bad entry. The controller applies the plugin's rules first, so what it serves always loads.              |
| Two Services claim the same tenancy and service           | The older `creationTimestamp` wins, then namespace and name. The other gets a Warning Event. | Deterministic, and a newcomer cannot take over traffic that is already flowing.                                                                      |
| Several Services of one tenancy, different TTLs           | The earliest expiry is the tenancy's                                                         | The registry has one expiry per tenancy. Ending early is the safe error.                                                                             |
| A SUT with no ready endpoints                             | Stays in the registry                                                                        | The plugin answers 502. Leaving it out would send the request to production and hide the failure.                                                    |
| `ExternalName` Services                                   | Refused                                                                                      | They would make the registry point outside the cluster, past any allowlist written for it.                                                           |
| Telling the user what happened                            | Events and metrics                                                                           | Writing back to the Service would fight whatever applies it, a GitOps tool above all.                                                                |
| More than 16 services in a tenancy                        | The 17th and later are left out, with an Event                                               | The plugin's limit.                                                                                                                                  |

## Who may claim a tenancy

**This is the part that most needs scrutiny.** Anyone who can create or edit
a Service the controller watches can point a tenancy's traffic at a workload
of theirs: a tester's requests, with their cookies and tokens, to the wrong
place.

The defences, in layers:

1. **Namespaces.** The controller watches the namespaces it is given, not the
   cluster. The default is its own.
2. **Policy per namespace**, in the controller's configuration: which tenancy
   prefixes a namespace may claim, and which services it may stand in for.

   ```yaml
   namespaces:
     siding-sut:
       tenancies: ['test/']
       services: ['cats-openapi', 'dogs-api']
     team-payments-sut:
       tenancies: ['test/payments/']
       services: ['payments']
   ```

3. **`tenancyPrefix`.** A tenancy outside the plugin's prefix is refused by
   the controller, as the plugin would never route it.
4. **`allowedHosts`**, in the plugin, is the last word. It bounds where a
   request can be sent whatever the registry says. With SUTs in namespaces of
   their own, `.siding-sut.svc:8080` covers them all and nothing else.
5. **Admission**, for clusters that want it enforced at write time: a
   `ValidatingAdmissionPolicy` that lets only certain identities set the
   `sut` label. The project documents one; it does not install one.

What this does not defend against: someone with write access to a namespace
the policy allows. That is the same trust as letting them deploy there.

## Keeping the allowlist and the registry in step

The plugin's `allowedHosts` and the controller's namespaces say the same
thing twice. The chart renders both from one value:

```yaml
discovery:
  namespaces:
    siding-sut:
      port: 8080
```

becomes `.siding-sut.svc:8080` in every Middleware's `allowedHosts`, and
`siding-sut` in the controller's configuration.

## Accounts

`accounts` binds a test account to a tenancy, so a logged-in tester routes
without sending baggage. An annotation on a Service is the wrong place for
it: an account belongs to a tenancy, not to one of its services.

- **First:** chart values, rendered into the controller's configuration.
- **Later:** the `Tenancy` resource.

## Later: a `Tenancy` resource

For what annotations express badly: accounts, one expiry, and a status.

```yaml
apiVersion: siding.rainbows.house/v1alpha1
kind: Tenancy
metadata:
  name: pr-123
  namespace: siding-sut
spec:
  tenancy: test/pr-123
  ttl:
    duration: 24h
    offsetFrom: createdAt # or updatedAt
  accounts:
    - tester-1
  services:
    - name: cats-openapi
      backendRef:
        name: cats-openapi-pr-123
        port: 8080
status:
  expiresAt: '2026-10-01T00:00:00Z'
  conditions:
    - type: Ready
      status: 'True'
  services:
    - name: cats-openapi
      url: http://cats-openapi-pr-123.siding-sut.svc:8080
```

- **Annotations keep working.** A `Tenancy` and annotated Services that name
  the same tenancy are merged; where they disagree the `Tenancy` wins.
- **The CRD is in the chart's `crds/` directory**, which Helm installs once
  and a GitOps tool can be told to skip. A cluster whose GitOps may not
  create CRDs applies it by hand and uses everything else as before.
- **The controller still deletes nothing.** An expired tenancy stops
  routing. Removing its workloads is for whatever created them.

## What was considered and not chosen

| Alternative                                          | Why not                                                                                                                                                                                          |
| ---------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| The plugin watches the Kubernetes API itself         | Traefik's catalog runs the plugin outside any cluster before listing it. It would need permissions on Traefik's own service account. And a watch is a lot to run interpreted, inside the proxy.  |
| The controller writes the registry into Middlewares  | Every change would rebuild Traefik's handlers. A GitOps tool would see the Middleware drift from what it applied, and revert it.                                                                 |
| The controller writes a ConfigMap, mounted as a file | The kubelet takes up to a minute to update a mounted ConfigMap, and the file has to be mounted into Traefik's pods.                                                                              |
| Derived HTTPRoutes that match on the baggage header  | Gateway API matches a header as a whole, so one baggage member needs a regular expression, which is implementation-specific. A more specific production route can also win over the derived one. |
| A CRD from the start                                 | Annotations need nothing installed, and cover the common case. The resource is better designed after the annotations have been used.                                                             |

## Open questions

- **Cross-namespace Traefik.** The Middleware, and so the plugin, is in
  Traefik's namespace. Is there a case for SUTs the controller cannot reach
  by DNS from there?
- **Headless Services.** Refuse them, or resolve them to pod addresses?
- **Should a not-ready SUT be visible in the registry?** A field the plugin
  ignores today would let the debug header say "the SUT is not ready" rather
  than a bare 502. The plugin refuses unknown fields, so this needs a plugin
  release first.
- **One controller for several Traefik installations**, or one each? `class`
  allows either; the chart has to pick a default.
