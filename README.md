# fxfiles-analytics

Minimal click-tracking backend for AI-generated static websites served from public IPFS gateways. Pairs with the **FxFiles** Flutter app and the AI generation backend at `ai.cloud.fx.land`.

This service is **stateless, cookieless, and GDPR-friendly by default** — no JWT, no user accounts, no tokens, no PII. Each generated site is identified by its **IPFS CID**, which is already in the gateway URL. The injected script self-discovers the CID from `window.location` and pings here; the FxFiles app reads aggregate counts back keyed by the same CID. Anyone with the CID can submit pings or read stats — which is fine, because the URL **is** the CID.

## Why this exists

FxFiles users generate static websites that get pinned to IPFS and served from public gateways like `dweb.link`. When they enable click-tracking, the AI generation backend injects a single inline `<script>` into the generated HTML that POSTs pageview pings here. The FxFiles app reads aggregated counts via a GET endpoint and shows them next to the generated link.

Privacy posture summary:

- **No cookies, no localStorage, no fingerprinting.** GDPR consent banner not required in most jurisdictions when configured per defaults; the deployer still needs a privacy notice.
- **Raw IP is never persisted.** A daily-rotating salt hash of `(IP || UA)` collapses repeat visits from the same client on the same day to one row.
- **Referrer is accepted but discarded by the reference impl.** The `/track` endpoint reads `ref` from the payload (so the wire format already carries it), but `recordPageview` does not persist it. A future revision can bucket it to the registrable domain via the public-suffix list; until then, no referrer data is stored.
- **CID is the only identifier.** It's public (it's the URL) — there's no separate secret to leak.

## API contract

### `POST /api/v1/track`

Sent by the visitor's browser when a tracked page loads.

Request:
```json
{
  "cid":   "<bafy...|Qm...>",
  "event": "pageview",
  "ref":   "<document.referrer || ''>"
}
```

Headers (advisory but enforced when present):
- `Origin` / `Referer` should end with `.ipfs.dweb.link` (or another allow-listed gateway domain). Requests that present a header with a non-matching host are rejected with `403`.

Behaviour:
- Reject `400` if `cid` doesn't match the basic CIDv0/CIDv1 shape (see `cidPattern` in `main.go`).
- Reject `429` if per-CID-per-IP rate-limit is exceeded (default 60 pings/min).
- Drop (return `204` silently) requests whose `User-Agent` is empty or matches the bot allow-list.
- Lazily create a record for `cid` on first sight, then increment the `pageviews` counter.
- Compute `sha256(daily_salt || ip || ua)`, truncate to 8 bytes hex, and add to the per-day `unique_visitors` set.
- Always returns `204 No Content` on success. The reference impl does **not** persist `ref`.
- Returns `503 Service Unavailable` if the distinct-CID cap (`MAX_DISTINCT_CIDS`, default 100k) has been reached and the CID is new — bounds storage growth from spoofing.

### `GET /api/v1/stats/{cid}`

Sent by the FxFiles app to populate the per-generation stats line.

Response (`200 OK`):
```json
{
  "pageviews":       142,
  "uniqueVisitors":   87
}
```

- `400` if the path segment doesn't match the CID shape.
- Returns `200` with `{"pageviews": 0, "uniqueVisitors": 0}` for CIDs no `/track` ping has hit yet — so the app shows zeroes rather than an "unavailable" error.
- No `Authorization` header.

## Injection snippet (for the AI generation backend)

When the FxFiles app sends `enable_tracking: true` in `POST /generate`, the generation backend must append the following `<script>` tag inside the `<head>` (or just before `</body>`) of the generated HTML, with `__ENDPOINT__` substituted:

```html
<script>
(function () {
  var ENDPOINT = '__ENDPOINT__/api/v1/track';
  try {
    // Self-discover the IPFS CID from window.location. Handles subdomain-
    // style (`{cid}.ipfs.<gateway>`) and path-style (`<gateway>/ipfs/{cid}/`).
    var cid = '';
    var parts = location.hostname.split('.');
    if (parts.length >= 3 && parts[1] === 'ipfs') {
      cid = parts[0];
    } else {
      var m = location.pathname.match(/^\/ipfs\/([^\/]+)/);
      if (m) cid = m[1];
    }
    if (!/^(Qm[1-9A-HJ-NP-Za-km-z]{44}|baf[ykz][a-z0-9]{40,80})$/.test(cid)) return;

    var data = JSON.stringify({
      cid: cid,
      event: 'pageview',
      ref: (document.referrer || '').slice(0, 200)
    });
    // 'text/plain' Content-Type is CORS-safelisted — sendBeacon delivers
    // without preflight, and fetch (no-cors) keeps the header. Server
    // json.Decode's the body regardless of the header value.
    var blob = new Blob([data], { type: 'text/plain' });
    if (navigator.sendBeacon && navigator.sendBeacon(ENDPOINT, blob)) return;
    fetch(ENDPOINT, {
      method: 'POST',
      headers: { 'Content-Type': 'text/plain' },
      body: data,
      keepalive: true,
      mode: 'no-cors'
    }).catch(function () {});
  } catch (e) {}
})();
</script>
```

The script:
- Self-discovers the CID — there's nothing for the backend to compute or substitute beyond the endpoint URL.
- Skips silently if the host isn't a recognized IPFS gateway URL (e.g. someone mirrors the HTML to a non-IPFS host).
- Uses `navigator.sendBeacon` when available so the ping survives page-unload.
- Falls back to `fetch` with `keepalive: true` and `mode: 'no-cors'`.
- Trims `document.referrer` to 200 chars to bound the field.
- Catches all exceptions so a tracking failure never breaks the user's page.

## Spoofing posture

The CID is public — it's the URL. Anyone can:

- Submit `/track` pings for any CID and inflate counts (mitigated by per-IP rate limit, bot UA filter, and the distinct-CID cap).
- Read `/stats/{cid}` for any CID (acceptable — the site is public and the counts are too).

The CID is the only identifier the service uses; there is no other secret to leak.

## Build & run

```bash
go run ./...
```

Environment variables:

| Var | Default | Notes |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | Bind address. |
| `DATA_DIR` | `./data` | JSON state file lives here. |
| `ALLOWED_GATEWAYS` | `.ipfs.dweb.link,.ipfs.cloud.fx.land` | Comma-separated `Referer`/`Origin` suffixes that `/track` accepts. |
| `RATE_LIMIT_PER_MIN` | `60` | Per-IP-per-CID cap. |
| `MAX_DISTINCT_CIDS` | `100000` | Hard cap on distinct CIDs in the store. New pings beyond the cap return `503`. |
| `DAILY_SALT_FILE` | `$DATA_DIR/.salt` | Where the rotating salt is persisted. Rotates at midnight UTC. |

```bash
docker build -t fxfiles-analytics .
docker run -p 8080:8080 \
  -v $PWD/data:/app/data \
  fxfiles-analytics
```

## Status

This is a **reference implementation** — minimal, single-binary, JSON-on-disk storage. Production deploys should replace the storage layer with PostgreSQL or Redis, add structured logging, and front it with a reverse proxy that terminates TLS and handles request-body limits.
