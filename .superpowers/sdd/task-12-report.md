# Task 12 — Embed page: vendor noVNC + boot.js

## Vendored noVNC

- **Package:** `@novnc/novnc`
- **Version:** `1.7.0` (confirmed current via `npm view @novnc/novnc version`)
- **How fetched:** `npm pack @novnc/novnc@1.7.0` in the scratchpad, extracted the
  tarball, copied `core/` (RFB client + full dependency tree: crypto, decoders,
  input, display, websock, util) and `vendor/pako/` (zlib inflate/deflate,
  imported by `core/inflator.js`/`core/deflator.js` for the tight/zrle
  decoders) verbatim into `ui/vendor/novnc/`, plus `LICENSE.txt`/`AUTHORS`.
  No build step run — npm's published source is already browser-usable ES
  modules (`package.json` `"type": "module"`, `"exports": "./core/rfb.js"`).
  Excluded: `docs/`, `README.md`, `package.json`, the demo `app/`, tests, and
  build tooling — none of it is needed to load `RFB` as an ES module.
- **Pin record:** `ui/vendor/novnc/VERSION` (includes tarball shasum and a
  short note on how to re-vendor a newer version).
- Total vendored: 58 files, ~715 KB — expected and accepted per the plan's
  explicit "vendored, built bundle in ui/" design.

## F-7 resolution (tunnelUrl discovery)

Not a guess in the end — found reasonably strong (though not 100% explicit)
evidence in `docs/plugin-api.md`:

- Lines 210–211 ("Session embed lifecycle"): "Host emits `SessionEmbedReady`
  to the UI with `uiUrl`, `tunnelUrl`, `sandbox`" then "Frontend mounts
  `SessionEmbedPanel`: iframe loads `uiUrl`; embed page opens WebSocket to
  `tunnelUrl`."
- Lines 234–236 (`session.registerEmbed` response shape):
  `"uiUrl": "/embed/s/<token>/ui/index.html"`,
  `"tunnelUrl": "/embed/s/<token>/tunnel/main"`.

Both URLs share the same `/embed/s/<token>/` prefix and are minted together.
Since the iframe is loaded AT `uiUrl` (i.e. our own `window.location` IS
`uiUrl` by the time `boot.js` runs), `tunnelUrl` is mechanically derivable
from our own location: swap the `/ui/...` segment for `/tunnel/main`. This is
the plan's "strategy (b)" guess, but now grounded in the exact documented URL
shapes rather than pure speculation. Also notable: the doc's explicit list of
host→iframe postMessages (line 267 area: `embed.viewport`, `embed.suspend`,
`embed.resume`) does **not** include a tunnelUrl message — further evidence
against a postMessage-only design.

Given this isn't a byte-for-byte authoritative statement ("the iframe derives
tunnelUrl from its own path" is not spelled out as a rule anywhere), I kept
the dual-strategy approach from the task brief as a safety net, implemented
in `ui/boot.js`:

1. **(a) postMessage** — listens briefly (1500 ms) for
   `{tunnelUrl: "..."}` (optionally tagged `source: "xquakshell-host"`) from
   the parent frame. Preferred if it arrives, since a real host push would be
   authoritative.
2. **(b) location-derivation** — replaces the last `/ui/` path segment with
   `/tunnel/main` and swaps `http(s):` → `ws(s):`. Used as the fallback after
   the timeout, backed by the doc evidence above.

Each path logs to the console explicitly stating which one fired and flagging
it as unconfirmed against a live host, per the task brief. **This remains
the single largest unresolved item** — a later phase should observe real host
behavior and delete whichever path never fires.

## RFB usage — verified against actual vendored source (not guessed)

Checked `ui/vendor/novnc/core/rfb.js` directly:

- `export default class RFB extends EventTargetMixin` (line 90) — confirms
  `import RFB from './vendor/novnc/core/rfb.js'` used in `boot.js` matches
  the real default export.
- Constructor: `constructor(target, urlOrChannel, options)` (line 91), with
  `options.credentials`, `options.shared` (default `true`), `options.wsProtocols`,
  `options.repeaterID` (lines 117–121) — `boot.js` calls
  `new RFB(containerEl, tunnelUrl, { shared: true })` and deliberately omits
  `credentials` (see below).
- `viewOnly` is a settable property (`get`/`set viewOnly`, lines 310–332), not
  a constructor option — `boot.js` sets `rfb.viewOnly = readOnly` after
  construction, matching this.
- Event names/detail shapes confirmed by grepping `dispatchEvent(new
  CustomEvent(...))` call sites:
  - `"connect"` with `{}` detail (line 930).
  - `"disconnect"` with `{ clean }` detail (lines 943–945).
  - `"credentialsrequired"` with `{ types: [...] }` detail (multiple sites,
    e.g. 1652–1654, 1756–1758).
  - `"securityfailure"` with `{ status, reason? }` detail (lines 1629–1640).
  - All four are wired up in `boot.js` with handlers matching these exact
    shapes.

## readOnly UX handling

Read from a `readOnly` query param on the iframe's own URL (documented
postMessages don't carry it either, so same "best effort, log which path
fired" treatment is not needed here since there's only one plausible source
given the doc's silence — kept it simple: query param only, defaulting to
`false`). Sets `rfb.viewOnly`, which per the vendored source stops noVNC's
own client from sending mouse/keyboard messages — reiterated in comments that
this is UX only; the real enforcement is server-side in
`internal/relay/readonly.go` per the design doc.

## No secrets

`boot.js` never references, reads, or constructs anything password-shaped.
The `RFB` options object passed omits `credentials` entirely. The
`credentialsrequired` handler treats firing as a bug signal (broken §1
synthetic-handshake invariant) and logs an error — it does not prompt for or
collect a password.

## Verification performed

1. `npm view @novnc/novnc version` → confirmed `1.7.0` is current before pinning.
2. `npm pack @novnc/novnc@1.7.0` → downloaded and inspected the real tarball
   contents (66 files listed by npm) to decide what's runtime-necessary vs.
   tooling/docs/tests.
3. `node --check <file>` on **all 54** vendored `.js` files under
   `ui/vendor/novnc/` (`core/` + `vendor/pako/`) — all pass, confirms they're
   syntactically valid.
4. `node --check ui/boot.js` — passes.
5. Attempted `node -e "import('./ui/vendor/novnc/core/rfb.js')..."` — fails at
   runtime with `window is not defined`, then (after stubbing a minimal
   `window`/`document`/`navigator`) with `MutationObserver is not defined`.
   This is expected: `rfb.js` and its dependency tree are genuinely
   browser-only (DOM APIs, ResizeObserver, MutationObserver, WebSocket, etc.)
   and cannot fully load under plain Node without a much heavier DOM shim
   (e.g. jsdom, not installed/requested). Syntax validity (`--check`) plus
   manual API verification via grep (constructor signature, `viewOnly`
   accessor, event names/detail shapes — all shown above with line numbers)
   is what's actually achievable and meaningful here; genuine runtime
   behavior can only be exercised in a real browser per the task's own
   framing ("this file should only be exercised by a real browser later").
6. Manually traced `boot.js` against the grepped `rfb.js` source line-by-line
   for every RFB API surface used (constructor args, `viewOnly`, the four
   event names and their `detail` fields) — no invented API surface.

## Concerns

- F-7 is still not a hard authoritative answer — it's strong circumstantial
  evidence (shared URL prefix in the documented response shape) plus absence
  of tunnelUrl from the documented postMessage list. Flagged clearly in
  `boot.js` comments and console logs. Needs live-host confirmation in a
  later phase (design doc §11 item 8).
- `readOnly` query-param delivery mechanism is also unconfirmed (no doc
  mentions how `fields.readOnly` reaches the embed page) — kept minimal
  (query param) rather than adding a second speculative postMessage path,
  since the cost of being wrong here is UX-only (server-side enforcement is
  unaffected either way).
- Runtime behavior of the vendored noVNC bundle in an actual browser was not
  and could not be exercised in this environment (no browser test harness
  available) — verification here is syntax + static API-surface tracing
  only, as noted above.

## Status

DONE_WITH_CONCERNS — deliverables complete and internally verified as far as
tooling allows; F-7 (tunnelUrl discovery) and the readOnly delivery
mechanism remain best-effort/unconfirmed against a live xQuakShell host, as
flagged throughout the code and this report.
