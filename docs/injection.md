# How the AI generation backend injects the analytics script

When the FxFiles app sends `POST /generate` with `enable_tracking: true`, the AI generation backend must append the script below to the generated HTML before pinning to IPFS.

That's it — no token, no registration step, no shared secret. The script self-discovers the CID from `window.location`, so it works for any generated site as long as it's served via an IPFS gateway whose URL contains the CID.

## Substitution

Only one placeholder:

- `__ENDPOINT__` → the analytics base URL with no trailing slash (e.g. `https://analytics.cloud.fx.land`).

## Where to put the script

Either inside `<head>` (so the ping fires early) or just before `</body>` (so it doesn't block first paint). Either works. If the generated HTML has neither a `<head>` nor a `<body>` (unlikely for a complete page), append at the end of the document — the script tolerates a partial DOM.

## The snippet

```html
<script>
(function () {
  var ENDPOINT = '__ENDPOINT__/api/v1/track';
  try {
    // Self-discover the IPFS CID from window.location.
    // Subdomain-style: `{cid}.ipfs.<gateway>`  →  hostname.split('.')[0]
    // Path-style:      `<gateway>/ipfs/{cid}/` →  pathname after /ipfs/
    var cid = '';
    var parts = location.hostname.split('.');
    if (parts.length >= 3 && parts[1] === 'ipfs') {
      cid = parts[0];
    } else {
      var m = location.pathname.match(/^\/ipfs\/([^\/]+)/);
      if (m) cid = m[1];
    }
    // If we can't recognize a CID, this isn't an IPFS gateway URL — skip
    // silently so mirrors and local previews don't pollute the store.
    if (!/^(Qm[1-9A-HJ-NP-Za-km-z]{44}|baf[ykz][a-z0-9]{40,80})$/.test(cid)) return;

    var data = JSON.stringify({
      cid: cid,
      event: 'pageview',
      ref: (document.referrer || '').slice(0, 200)
    });
    // Content-Type 'text/plain' is CORS-safelisted — sendBeacon delivers
    // without preflight, and fetch in no-cors mode keeps the header. The
    // body is still valid JSON; the analytics backend json.Decode's the
    // bytes regardless of header.
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

## Reliability notes

- **CID self-discovery** works for both gateway URL styles. If the script can't recognize a CID (someone copies the HTML to a non-IPFS host, runs it locally, etc.), it skips silently — no spurious data.
- **`navigator.sendBeacon`** is the preferred path: the browser will complete the request even if the user closes the tab immediately after load.
- **`fetch` with `keepalive: true`** is the fallback for browsers without sendBeacon support.
- **`Content-Type: text/plain`** is intentional. `application/json` is **not** on the CORS-safelisted-request-Content-Type list — sendBeacon would coerce or drop the request silently, and a `no-cors` fetch would strip the header. `text/plain` clears both checks. The body is well-formed JSON; the analytics backend reads bytes via `json.Decode` and never inspects the Content-Type header.
- **`mode: 'no-cors'`** lets the request fire even when the analytics endpoint doesn't return CORS headers — the response is opaque, which is fine because we don't need to read it.
- Every code path is wrapped in `try/catch` and `.catch(function() {})` so a tracking failure can never break the user's page.

## What the snippet does NOT do

- No cookies, no localStorage, no IndexedDB.
- No `Set-Cookie` headers from the response.
- No fingerprinting (canvas, WebGL, font enumeration).
- No third-party script load — the whole snippet is inline.
- No PII collection (the only identifier is the IPFS CID, which is already in the URL).

## What the analytics backend does with the ping

See `../README.md` for the full contract. Short version:

- Verifies the CID matches a basic CIDv0/CIDv1 shape.
- Verifies `Origin`/`Referer` ends with an allow-listed gateway suffix.
- Drops obvious bot UAs.
- Lazily creates a record for the CID on first sight, then increments the pageview counter.
- Computes `sha256(daily_salt || ip || ua)`, truncates to 8 bytes, adds to the day's `unique_visitors` set. Raw IP is never persisted; the salt rotates at UTC midnight.
- Reads the `ref` field off the wire but does **not** persist it in the reference impl (future revisions can bucket it to a registrable domain).
- Returns `204 No Content`.
