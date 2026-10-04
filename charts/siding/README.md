# siding Helm chart

Renders one Traefik `Middleware` for each service you guard with
[siding](https://github.com/RainbowsHouse/siding).

```sh
helm repo add siding https://rainbowshouse.github.io/siding
helm install siding siding/siding --namespace traefik --values values.yaml
```

## What it does not do

**It does not load the plugin into Traefik.** That is Traefik's static
configuration, which belongs to however Traefik itself is installed. Do that
first, by either way in the
[project README](https://github.com/RainbowsHouse/siding#install): a
`Middleware` whose plugin Traefik does not know costs the route it is
attached to.

It does not attach a Middleware to a route either. That is yours to do, where
you attach any Traefik middleware:

```yaml
# a rule of an HTTPRoute, in the Middleware's namespace
filters:
  - type: ExtensionRef
    extensionRef:
      group: traefik.io
      kind: Middleware
      name: siding-cats-openapi
```

Where a route also has a forwardAuth middleware, put siding after it, so that
`userHeader` holds what forwardAuth wrote and not what the client sent.

## Values

| Key                 | Default   | Meaning                                                                                          |
| ------------------- | --------- | ------------------------------------------------------------------------------------------------ |
| `services`          | `{}`      | The services to guard, one Middleware each. The key is the name the service has in the registry. |
| `defaults`          | `{}`      | Plugin configuration shared by every service.                                                    |
| `plugin.alias`      | `siding`  | The name the plugin has in Traefik's static configuration.                                       |
| `namePrefix`        | `siding-` | What each Middleware's name starts with.                                                         |
| `commonLabels`      | `{}`      | Labels added to every Middleware.                                                                |
| `commonAnnotations` | `{}`      | Annotations added to every Middleware.                                                           |

`defaults`, and the value of each entry under `services`, take the plugin's
[configuration keys](https://github.com/RainbowsHouse/siding#configuration),
except `service`.

```yaml
defaults:
  registry:
    url: http://registry.example.svc:8080/registry
  allowedHosts:
    - '.siding-sut.svc:8080'
services:
  cats-openapi: {}
  dogs-api:
    userHeader: Remote-User
```

- **A key set for a service replaces the default's, whole.** A service that
  sets `registry.url` does not inherit a `registry.file` from `defaults`,
  which the plugin would refuse as two sources.
- **A misspelt key fails the install.** Traefik itself drops a key it does
  not know without a word; the chart's schema refuses it.
- **Quote `allowedHosts` entries and `expiresAt`**, as anywhere in YAML.

More in [examples/](examples).

## Versions

The chart has the plugin's version: `siding-0.3.0` configures plugin `v0.3.0`.
Upgrade the two together. With the plugin loaded locally, that is this
chart's version and the name of the ConfigMap Traefik mounts.
