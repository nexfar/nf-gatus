# Endpoint `id` (Nexfar fork)

Gatus derives every endpoint's unique key from its group and name
(`sanitize(group)_sanitize(name)`). That key is how the endpoint is identified
everywhere: storage rows (history/uptime), the dashboard API
(`/api/v1/endpoints/{key}/...`), badge URLs and — for external endpoints — the
push URL (`POST /api/v1/endpoints/{key}/external`).

The consequence upstream is that renaming an endpoint changes its key: history
resets, badge URLs change and, for external endpoints, every pusher breaks.

This fork adds an optional `id` field to `endpoints` and `external-endpoints`.
When set, the key is derived from the group and the `id` instead of the name,
so the name becomes a purely cosmetic display name that can be changed freely:

```yaml
external-endpoints:
  - name: Integrador            # display name, rename at will
    id: liveness                # key stays <group>_liveness
    group: navarromed
    token: "${TOKEN}"

endpoints:
  - name: Plataforma
    id: site                    # key stays <group>_site
    group: navarromed
    url: https://navarromed.nexfar.com.br
```

Notes:

- The `id` goes through the same sanitization as names (lowercased, most
  punctuation replaced with `-`), and the group remains part of the key — so
  tenant scoping (see [multi-tenancy.md](multi-tenancy.md)) is unaffected.
- When an endpoint with an `id` is renamed, the stored display name is synced
  at startup and on configuration reload; history, events and uptime are
  preserved.
- Adding an `id` to an endpoint that already has history under its name-derived
  key behaves like a rename does upstream (the key changes once). Pick
  `id: <previous name>` to keep the existing key and history.
- When `id` is empty the behavior is identical to upstream.
