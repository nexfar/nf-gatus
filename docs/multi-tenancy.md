# Multi-tenant dashboards by subdomain

This is a Nexfar-specific addition to Gatus. It lets a single deployment serve a
**per-tenant view of the dashboard based on the request's subdomain**, so each
tenant only sees the endpoints/suites that belong to their group.

- `status.nexfar.com.br` (the apex/root) → **full view**, every group.
- `navarromed.status.nexfar.com.br` → only the `navarromed` group.
- `plena.status.nexfar.com.br` → only the `plena` group.

The filtering is enforced **server-side**, so it is a real access boundary: a tenant
cannot read another group's data by reading the raw JSON API or by guessing an
endpoint key.

## Table of contents

- [How it works](#how-it-works)
- [Configuration](#configuration)
- [Naming your groups](#naming-your-groups)
- [Behavior reference](#behavior-reference)
- [Security model](#security-model)
- [DNS and TLS](#dns-and-tls)
- [Deploying the image](#deploying-the-image)
- [The publish-image workflow](#the-publish-image-workflow)

## How it works

A request's host is mapped to a **tenant slug**:

1. The configured `root-domain` suffix is stripped from the request host.
2. Whatever single label remains is the tenant (e.g. `navarromed`).
3. That label is sanitized the same way group names are (lowercased, spaces and
   special characters become `-`) and matched against each endpoint/suite's group.

Because an endpoint's storage key is `sanitize(group) + "_" + sanitize(name)`, the
group portion of the key is matched against the tenant slug. The apex domain, plus
any host that is not a subdomain of `root-domain` (localhost, an IP, a direct
hostname during development), is treated as **unscoped** and sees everything.

## Configuration

Add a `tenancy` section to your Gatus config:

```yaml
tenancy:
  root-domain: status.nexfar.com.br
```

| Key           | Type   | Default | Description |
|:--------------|:-------|:--------|:------------|
| `root-domain` | string | `""`    | The apex domain the dashboard is served from, **without** any subdomain. When empty (or the `tenancy` section is omitted), multi-tenancy is disabled and every request sees every group. |

That is the only setting. New tenants need **no config change** — just create endpoints
whose group matches the desired subdomain (see below).

## Naming your groups

The subdomain label must match the **sanitized** group name. Sanitization lowercases
the value and replaces spaces, `/`, `_`, `.`, `,`, `#`, `+` and `&` with `-`.

| Group name      | Sanitized slug | Subdomain that sees it                    |
|:----------------|:---------------|:------------------------------------------|
| `navarromed`    | `navarromed`   | `navarromed.status.nexfar.com.br`         |
| `Plena`         | `plena`        | `plena.status.nexfar.com.br`              |
| `Navarro Med`   | `navarro-med`  | `navarro-med.status.nexfar.com.br`        |

Example endpoints:

```yaml
tenancy:
  root-domain: status.nexfar.com.br

endpoints:
  - name: api
    group: navarromed
    url: "https://api.navarromed.example.com/health"
    conditions:
      - "[STATUS] == 200"

  - name: api
    group: plena
    url: "https://api.plena.example.com/health"
    conditions:
      - "[STATUS] == 200"
```

With the config above:
- `navarromed.status.nexfar.com.br` shows only `navarromed / api`.
- `plena.status.nexfar.com.br` shows only `plena / api`.
- `status.nexfar.com.br` shows both.

## Behavior reference

| Request host                              | Sees                       |
|:------------------------------------------|:---------------------------|
| `status.nexfar.com.br` (apex)             | All groups (full view)     |
| `navarromed.status.nexfar.com.br`         | Only the `navarromed` group |
| `unknown.status.nexfar.com.br`            | Nothing (no group matches)  |
| `localhost` / IP / any non-matching host  | All groups (full view)      |

## Security model

- Filtering happens in the API layer, before any data leaves the server.
- The following routes are tenant-scoped and return **404** for a key that does not
  belong to the requesting subdomain (so the existence of other tenants' endpoints is
  not leaked):
  - `GET /api/v1/endpoints/statuses` (filtered list)
  - `GET /api/v1/endpoints/:key/statuses`
  - `GET /api/v1/endpoints/:key/health/badge.svg` and `.../health/badge.shields`
  - `GET /api/v1/endpoints/:key/uptimes/:duration` (+ `/badge.svg`)
  - `GET /api/v1/endpoints/:key/response-times/:duration` (+ `/badge.svg`, `/chart.svg`, `/history`)
  - `GET /api/v1/suites/statuses` and `GET /api/v1/suites/:key/statuses`
- **The apex domain is currently unauthenticated** and shows everything. If the root
  dashboard should be restricted, add Gatus' built-in [security](https://github.com/nexfar/nf-gatus#security)
  (basic auth or OIDC) — it applies globally. Per-subdomain auth is not part of this
  feature yet.
- Endpoints **without a group** are only visible on the apex/unscoped view (their key
  has an empty group slug, which never matches a subdomain).

## DNS and TLS

To serve tenant subdomains from one deployment you need:

1. **Wildcard DNS** — `*.status.nexfar.com.br` pointing at the same host/load balancer
   as `status.nexfar.com.br`.
2. **A wildcard TLS certificate** covering `*.status.nexfar.com.br` (and the apex), on
   whatever terminates TLS in front of Gatus (ingress / reverse proxy / load balancer).

No per-tenant routing is required at the proxy layer — every subdomain hits the same
Gatus instance, which does the filtering based on the `Host` header. Make sure your
proxy forwards the original `Host` (e.g. nginx `proxy_set_header Host $host;`).

## Deploying the image

CI publishes a multi-arch image (linux/amd64, linux/arm64) to GitHub Container Registry:

```
ghcr.io/nexfar/nf-gatus:latest        # latest master build
ghcr.io/nexfar/nf-gatus:sha-<short>   # a specific commit
ghcr.io/nexfar/nf-gatus:1.2.3         # a released tag (when you push vX.Y.Z)
```

> **One-time setup:** GHCR packages are created **private** by default. To allow
> unauthenticated pulls, open the package at
> `https://github.com/orgs/nexfar/packages`, then **Package settings → Danger Zone →
> Change visibility → Public**, and **link it to the `nf-gatus` repository**. If you
> prefer to keep it private, the deploy environment must `docker login ghcr.io` with a
> token that has `read:packages`.

### docker compose example

```yaml
services:
  gatus:
    image: ghcr.io/nexfar/nf-gatus:latest
    restart: unless-stopped
    ports:
      - "8080:8080"
    environment:
      GATUS_CONFIG_PATH: /config/config.yaml
    volumes:
      - ./config.yaml:/config/config.yaml:ro
```

Pin to an immutable tag (`sha-<short>` or `vX.Y.Z`) in production rather than `latest`,
so deploys are reproducible. To cut a versioned release, push a tag:

```bash
git tag v1.0.0
git push origin v1.0.0
```

This triggers the workflow and publishes `ghcr.io/nexfar/nf-gatus:1.0.0`, `:1.0` and
refreshes `:latest`.

## The publish-image workflow

`.github/workflows/publish-image.yml` builds and pushes the image. It runs on:

- **push to `master`** (excluding doc-only changes) → `:latest` + `:sha-<short>`
- **push of a `v*` tag** → semver tags + `:latest`
- **manual dispatch** (Actions tab → *publish-image* → *Run workflow*)

It authenticates to GHCR with the built-in `GITHUB_TOKEN` (no secrets to configure).
The upstream TwiN `publish-latest` / `publish-release` / `publish-custom` /
`publish-experimental` workflows were removed because they target TwiN's Docker Hub.
