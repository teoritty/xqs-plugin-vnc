// ui/boot.js — mounts noVNC into the embed iframe (embedEntry: ui/vnc.html).
//
// Vendored noVNC: @novnc/novnc 1.7.0, see ui/vendor/novnc/VERSION.
//
// ---------------------------------------------------------------------------
// F-7 (open finding, docs/superpowers/specs/2026-07-16-vnc-plugin-design.md
// §10): "How the embed iframe learns tunnelUrl" is not spelled out as an
// explicit step in docs/plugin-api.md's embed sequence. What *is* documented
// (docs/plugin-api.md, "Session embed lifecycle" + "session.registerEmbed"):
//
//   8. Host emits SessionEmbedReady to the UI with uiUrl, tunnelUrl, sandbox.
//   9. Frontend mounts SessionEmbedPanel: iframe loads uiUrl; embed page
//      opens WebSocket to tunnelUrl.
//
//   session.registerEmbed response shape:
//     { "embedToken": "<64-hex>",
//       "uiUrl":     "/embed/s/<token>/ui/index.html",
//       "tunnelUrl": "/embed/s/<token>/tunnel/main",
//       "expiresAt": "..." }
//
// uiUrl and tunnelUrl share the same "/embed/s/<token>/" prefix and are both
// minted together server-side from the same token. The iframe is loaded
// AT uiUrl (i.e. window.location IS uiUrl once this script runs), so the
// token is already present in our own location — tunnelUrl is mechanically
// derivable from it without any extra handoff. That is reasonably strong
// (though not 100% explicit/authoritative) evidence for strategy (b) below.
// The doc's explicitly-specified host->iframe postMessages are only
// embed.viewport / embed.suspend / embed.resume (docs/plugin-api.md,
// "Embed clients must listen for host -> iframe postMessage") — tunnelUrl is
// conspicuously NOT among them, which further supports "derive it, don't
// wait for it". Still, nothing rules out a host implementation that also
// pushes it via postMessage, so we defensively support both, preferring
// whichever resolves first and logging which one actually fired. A later
// phase should confirm this against a live host and delete whichever path
// didn't fire (see design doc §10 F-7 / §11 item 8).
// ---------------------------------------------------------------------------

import RFB from './vendor/novnc/core/rfb.js';

const POSTMESSAGE_TIMEOUT_MS = 1500;

const statusEl = document.getElementById('vnc-status');
const containerEl = document.getElementById('vnc-container');

function setStatus(text) {
  if (statusEl) statusEl.textContent = text;
}

function hideStatus() {
  if (statusEl) statusEl.style.display = 'none';
}

/**
 * Strategy (a): listen for a postMessage from the parent frame carrying the
 * tunnel URL. Shape is a guess — no doc confirms the host sends this at all
 * for tunnelUrl specifically (only embed.viewport/suspend/resume are
 * documented postMessages). We accept a couple of plausible shapes:
 *   { source: "xquakshell-host", type: "embed.tunnelUrl", tunnelUrl: "..." }
 *   { tunnelUrl: "..." }
 */
function waitForPostMessageTunnelUrl(timeoutMs) {
  return new Promise((resolve) => {
    let settled = false;

    function onMessage(event) {
      if (settled) return;
      const data = event.data;
      if (!data || typeof data !== 'object') return;
      const url = data.tunnelUrl;
      if (typeof url !== 'string' || url.length === 0) return;
      // Accept either an explicitly-tagged host message or a bare
      // {tunnelUrl} payload — the exact shape is unconfirmed (F-7).
      if (data.source && data.source !== 'xquakshell-host') return;
      settled = true;
      window.removeEventListener('message', onMessage);
      resolve(url);
    }

    window.addEventListener('message', onMessage);

    setTimeout(() => {
      if (settled) return;
      settled = true;
      window.removeEventListener('message', onMessage);
      resolve(null);
    }, timeoutMs);
  });
}

/**
 * Strategy (b): derive tunnelUrl from our own location. Per
 * docs/plugin-api.md, uiUrl and tunnelUrl are minted together as
 * "/embed/s/<token>/ui/index.html" and "/embed/s/<token>/tunnel/main" — same
 * "/embed/s/<token>/" prefix, "ui/..." replaced by "tunnel/main". We're
 * loaded at uiUrl (this page IS ui/vnc.html under that prefix), so we can
 * reconstruct it: swap the last "/ui/" path segment for "/tunnel/main" and
 * convert http(s) -> ws(s).
 */
function deriveTunnelUrlFromLocation() {
  const loc = window.location;
  const wsProtocol = loc.protocol === 'https:' ? 'wss:' : 'ws:';

  const uiSegmentIndex = loc.pathname.lastIndexOf('/ui/');
  if (uiSegmentIndex === -1) {
    return null;
  }
  const prefix = loc.pathname.slice(0, uiSegmentIndex); // ".../embed/s/<token>"
  const tunnelPath = `${prefix}/tunnel/main`;
  return `${wsProtocol}//${loc.host}${tunnelPath}`;
}

/**
 * readOnly UX flag (NOT the security boundary — see below). Documented
 * postMessages don't include a readOnly payload either, so same dual
 * approach: query param on our own uiUrl, or a postMessage field, whichever
 * arrives. Defaults to false (read-write) if neither is present.
 */
function readReadOnlyFromQuery() {
  const params = new URLSearchParams(window.location.search);
  const raw = params.get('readOnly');
  return raw === '1' || raw === 'true';
}

async function resolveTunnelUrl() {
  const postMessagePromise = waitForPostMessageTunnelUrl(POSTMESSAGE_TIMEOUT_MS);
  const locationUrl = deriveTunnelUrlFromLocation();

  // Race: if the host posts a tunnelUrl before the timeout, prefer it
  // (it's authoritative if it exists at all). Otherwise fall back to the
  // location-derived guess once the timeout elapses.
  const fromPostMessage = await postMessagePromise;
  if (fromPostMessage) {
    console.info(
      '[xqs-plugin-vnc] tunnelUrl resolved via postMessage from parent frame ' +
      '(F-7 strategy a). This is the documented-adjacent path; verify against ' +
      'the design doc §10 F-7 once host behavior is observed.',
      fromPostMessage
    );
    return fromPostMessage;
  }

  if (locationUrl) {
    console.info(
      '[xqs-plugin-vnc] tunnelUrl resolved by deriving from window.location ' +
      '(F-7 strategy b, no postMessage received within ' +
      POSTMESSAGE_TIMEOUT_MS + 'ms). Best-effort: uiUrl/tunnelUrl share a ' +
      '"/embed/s/<token>/" prefix per docs/plugin-api.md\'s ' +
      'session.registerEmbed response shape, but this derivation is NOT ' +
      'confirmed against a live host. Flag for follow-up per design doc §10 F-7.',
      locationUrl
    );
    return locationUrl;
  }

  throw new Error(
    '[xqs-plugin-vnc] Could not resolve tunnelUrl: no postMessage received ' +
    'and window.location does not contain a "/ui/" segment to derive from. ' +
    'window.location.href=' + window.location.href
  );
}

async function main() {
  let tunnelUrl;
  try {
    tunnelUrl = await resolveTunnelUrl();
  } catch (err) {
    console.error(err);
    setStatus('Failed to determine VNC tunnel URL.');
    return;
  }

  const readOnly = readReadOnlyFromQuery();

  // No password ever lives in this file. Per design doc §1, the plugin has
  // already fully consumed the real VNC password server-side (DES challenge
  // response to the real server) before the browser connects at all. From
  // noVNC's point of view here, the server offers ONLY security type
  // "None" and immediately returns SecurityResult=OK — there is no
  // credential for this file to hold, read, or transmit.
  const rfb = new RFB(containerEl, tunnelUrl, {
    shared: true,
    // credentials: intentionally omitted — none exist on this side.
  });

  // readOnly is UX-only here: it stops noVNC's own client from bothering to
  // send input as a courtesy, and could be used to show a "read-only"
  // indicator in the UI. The actual enforcement boundary is server-side, in
  // the Go plugin's internal/relay/readonly.go, which drops client input
  // messages before they ever reach the real VNC server — a compromised or
  // patched copy of this page cannot bypass that by setting viewOnly=false.
  rfb.viewOnly = readOnly;
  if (readOnly) {
    containerEl.classList.add('vnc-read-only');
  }

  rfb.addEventListener('connect', () => {
    hideStatus();
    console.info('[xqs-plugin-vnc] RFB connected.');
  });

  rfb.addEventListener('disconnect', (evt) => {
    const clean = evt && evt.detail ? evt.detail.clean : undefined;
    setStatus(clean ? 'Disconnected.' : 'Connection lost.');
    console.info('[xqs-plugin-vnc] RFB disconnected, clean=' + clean);
  });

  // This session is pre-authenticated by the plugin (design doc §1): the
  // synthetic preamble noVNC sees offers only security type None and an
  // immediate SecurityResult=OK. noVNC should never need real credentials
  // here. If this fires, the synthetic handshake invariant from §1 is
  // broken somewhere upstream (frontshake.go) — it is NOT a legitimate
  // "please enter your VNC password" prompt, and there is no password on
  // this side to enter. We deliberately do not show a password prompt for
  // it, since noVNC here has no way to validate anything the user would
  // type in.
  rfb.addEventListener('credentialsrequired', (evt) => {
    console.error(
      '[xqs-plugin-vnc] UNEXPECTED: RFB requested credentials on a session ' +
      'that should already be fully authenticated server-side (design doc ' +
      '§1 invariant). This indicates a bug in the plugin\'s synthetic RFB ' +
      'preamble, not a real auth prompt. types=',
      evt && evt.detail ? evt.detail.types : undefined
    );
    setStatus('Unexpected authentication request — this session should already be authenticated.');
  });

  rfb.addEventListener('securityfailure', (evt) => {
    const detail = evt && evt.detail ? evt.detail : {};
    console.error('[xqs-plugin-vnc] RFB security failure.', detail);
    setStatus('Connection failed: ' + (detail.reason || ('security status ' + detail.status)));
  });

  // Host -> iframe postMessage protocol (docs/plugin-api.md, "Embed clients
  // must listen for host -> iframe postMessage"): embed.viewport /
  // embed.suspend / embed.resume. Per design doc §7, suspend must NOT
  // disconnect the WebSocket — only pause rendering.
  let suspended = false;
  window.addEventListener('message', (event) => {
    const data = event.data;
    if (!data || typeof data !== 'object' || data.source !== 'xquakshell-host') {
      return;
    }
    switch (data.type) {
      case 'embed.viewport':
        // noVNC auto-scales/handles resize via its own ResizeObserver on
        // the target element; the container fills the iframe (see
        // ui/vnc.html CSS), so no explicit resize call is needed here.
        break;
      case 'embed.suspend':
        suspended = true;
        break;
      case 'embed.resume':
        suspended = false;
        break;
      default:
        break;
    }
  });
  void suspended; // reserved for a future pause-rendering hook if needed
}

main();
