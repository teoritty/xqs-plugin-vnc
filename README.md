# xqs-plugin-vnc

A VNC client for [xQuakShell](https://github.com/teoritty/xQuakShell). It adds a
`vnc` connection protocol and renders the remote desktop in an embedded
[noVNC](https://github.com/novnc/noVNC) viewer inside the session tab — mouse,
keyboard and clipboard included.

## What it does

- Connects to any RFB 3.8 VNC server (default port **5900**).
- Authenticates with the standard VNC password (DES challenge) **host-side** —
  the password never reaches the browser. The embedded viewer only ever sees a
  synthetic, already-authenticated handshake.
- **Fits the remote desktop to the window.** Where the server supports the
  Extended DesktopSize extension (TigerVNC, `x11vnc -xrandr`, …) it resizes the
  remote to match the panel; otherwise the image is scaled to fit, preserving
  aspect ratio.
- **Auto-reconnects** a dropped session (network change, VPN toggle, server
  restart) for up to 30 seconds before surfacing an error, keeping the tab.
- **Bandwidth vs. quality controls** per connection (see below).
- Optional **read-only** mode (input is dropped server-side, not just in the UI).

## Requirements

- xQuakShell with plugin API `1.0.0`.
- A reachable VNC server speaking **RFB 3.8** with security type **None** or
  **VNC password**. (Other security types are not implemented.)

## Install

Install through xQuakShell's plugin manager, or point it at a release from the
[Releases](https://github.com/teoritty/xqs-plugin-vnc/releases) page (each
release ships a `.xqsp` bundle per platform).

## Connection settings

| Field | Meaning |
|-------|---------|
| **Password** | VNC password. Stored encrypted in xQuakShell's vault; never sent to the browser. |
| **Read only** | View without sending input. Enforced server-side. |
| **Quality** (0–9) | JPEG quality for photographic/video regions of the Tight encoding. Lower = less bandwidth, softer image. |
| **Compression** (0–9) | zlib effort for the non-photographic regions. Lossless; higher = less bandwidth, more server CPU. |

### A note on bandwidth

VNC has no inter-frame (motion) compression the way a real video codec does — a
changing region is re-encoded every frame. Full-motion content (a video playing
on the remote) is therefore expensive on any VNC client, and **Quality** is the
only real lever to bring it down. For static/desktop work the difference between
quality 5 and 9 is barely visible while the bandwidth is far lower; for video,
try quality 4–5. **Compression** mostly helps UI and text, not video.

## How it works

The plugin process performs the real RFB handshake with the VNC server over a
host-dialed `tcp-relay` channel, then presents the browser a synthetic
pre-authenticated RFB preamble over an `embed-stream` channel. After both
handshakes it relays raw bytes between the two, so the encoding is negotiated
end-to-end between noVNC and the server and the plugin never decodes pixels. The
embedded viewer is served from a loopback origin and reached over a
per-session, token-gated WebSocket tunnel; the vendored noVNC runs in a
sandboxed iframe.

## Limitations

- RFB 3.8 only; security types None and VNC password only.
- VNC is a desktop-sharing protocol, not a video stream — expect high bandwidth
  for full-motion content regardless of settings.

## License

GPL-3.0.
