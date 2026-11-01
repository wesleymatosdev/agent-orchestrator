# Session replay harness (dev-only, round 1)

Plays a recorded AO session back through the **real renderer** — real React
components, real board, real chat — with the daemon's HTTP API shimmed at the
dev-server boundary. Nothing in the app is modified for replay; no user-facing
UI is added in this round.

Fidelity bar (owner requirement): a viewer must not be able to tell replay from
the live app. Static token-styled clones do not count.

## Run

```bash
cd frontend
npm run replay                 # serves http://127.0.0.1:4173 (fixtures below)
# open the printed orchestrator-chat URL, e.g.
#   http://127.0.0.1:4173/#/projects/bela-portfolio1/sessions/bela-portfolio1-44
```

The bundled fixture (`fixtures/`) replays the proven demo arc: project
`bela-portfolio1`, orchestrator session `bela-portfolio1-44` dispatching worker
session `bela-portfolio1-45` ("hero name letter arrival animations", 2026-11-01
14:09:30–15:40 UTC) — dispatch → real spawn-failure beat → worker spawned →
board card → worker conversation → PR #25 → routed feedback.

## Rebuild fixtures from ao.db (read-only) + fresh daemon captures

```bash
python3 scripts/replay/extract_timeline.py \
  --project-conv <project-conversation-id> \
  --worker-conv <session-conversation-id> \
  --session <worker-session-id> \
  --start "YYYY-MM-DD HH:MM:SS" --end "YYYY-MM-DD HH:MM:SS" \
  --capture-dir <dir with daemon GET captures> --out fixtures
```

ao.db is opened strictly `mode=ro`. The capture dir holds plain GETs recorded
from a running daemon (`projects.json`, `orch-session.json`,
`worker-session.json`, `<id>-conversation.json`, plus the optional
`perc/ep-*` peripheral responses the harness serves verbatim).

## Architecture: the shim seam

The renderer always talks to `/api/v1/*` over HTTP (in the packaged app these
go to the loopback daemon; in `dev:web` the vite dev server proxies them via
`AO_DEV_API_TARGET`). `replay-server.mjs` starts vite with the standard
renderer config plus one plugin whose middleware takes over those paths —
**the same seam the dev proxy owns**, so round 2 swaps the implementation
without touching the renderer contract:

- `GET /api/v1/sessions/{id}/conversation` — time-sliced
  `ConversationSnapshotResponse` (messages/activities filtered by sequence
  cutoff from the extracted timeline; turns appear at `requestedAt` and flip to
  the recorded final state at `completedAt`; `controller` is `busy` while any
  turn runs; turn timestamps are rewritten to replay wall time so relative
  labels read like a live session).
- `GET /api/v1/sessions/{id}` — time-sliced session detail (activity state,
  status, `kanbanColumn`, PR facts as of story time).
- `GET /api/v1/events` — the daemon's SSE CDC stream, replayed on schedule:
  each conversation beat emits `session_updated` with
  `payload.conversationId` (invalidates that conversation), lifecycle beats
  emit id-only `session_updated` (invalidates the workspace/board; spaced ≥6s
  to clear the client's id-only refresh throttle).
- `GET /__ao_replay/board` — board snapshot for the documented
  `window.__aoFakeAgent` preview seam (preview mode reads it per workspace
  fetch, so clock-driven lane transitions flow straight into the real
  `SessionsBoard`). This is preview-mode-only; the round-2 daemon endpoint
  replaces it because the real daemon path builds the workspace from
  `GET /api/v1/projects` + `GET /api/v1/sessions`.
- Everything else (`projects`, `agents/readiness`, `notifications`,
  conversation models/skills/config-options, reviews, secondary SSE streams)
  is served from captured responses or quiet stubs; unmatched paths 404 loudly
  (logged) so gaps are visible.

The plugin only injects `scripts/replay/boot.js` into `index.html`; boot
polls `/__ao_replay/board` on `setInterval` (never rAF — rAF pauses in
occluded/headless tabs) and exposes the shared story clock as
`window.__aoReplay` for the capture driver.

The story clock is piecewise ("time warps"): slow through the demo scenes
(orchestrator dispatch, board/card, PR open), fast across quiet work stretches,
so a 90-minute session compresses to ~2.5 minutes without losing readable
beats. Server, boot script, and SSE schedule integrate the same warp table.

## Round 2: the daemon `--replay` endpoint must serve

To move replay from the dev-server shim into the daemon (upstream `ao replay`),
the daemon needs, per replayed session:

1. `GET /api/v1/sessions/{sessionId}/conversation` — snapshot slicing exactly
   as above (sequence cutoff + turn state projection + wall-clock rewrite).
2. `GET /api/v1/sessions/{sessionId}` — projected session detail (activity
   state, derived status/kanbanColumn, PR facts as of story time).
3. `GET /api/v1/events` — CDC replay: `session_updated` frames with
   `payload.conversationId` at conversation beats, id-only frames at board
   beats (≥6s apart).
4. `GET /api/v1/projects` + `GET /api/v1/sessions` — projected lists (the real
   daemon workspace path; replaces the `__aoFakeAgent` seam).
5. Static per-session reads can pass through verbatim (models, skills,
   config-options, reviews, notifications).
6. Mutations (`POST .../send` etc.) should 202/`accepted` without side effects.

A `--replay <fixture>` mode that mounts a handler in front of the real routes
(and freezes real state access) is enough; the renderer needs no changes.

## Where the "export demo" button belongs (round 3 UI)

The natural home is the **session header's overflow menu** (the `⋯` control in
the session tab strip / session view header), as "Export demo replay…" —
session-scoped, where ownership of the conversation is unambiguous. A secondary
entry in the **project board's action row** ("Replay dispatch…") covers the
orchestrator-dispatch arc that starts from the project conversation, where the
spawned session's header doesn't exist yet.

## Options

- `REPLAY_SHOW_COMMANDS=0` (or `--show-commands 0`) hides command / file-change
  / mcp_tool activity rows from the replay; default **shown** (owner
  preference). The filter applies to the SSE schedule and the served snapshot
  activities, never to the sequence cutoff, so message timing is unchanged.
- `--scale <n>` multiplies the base story speed (the warp segments divide out
  of the same clock; the option is kept for simple fixtures without warps).

## Round-3 UI control

The activity-rows toggle belongs next to the export-demo entry: in the export
dialog (session header overflow menu → "Export demo replay…") as a checkbox
"Include command and file-change activity", defaulting to on; the board
action-row entry ("Replay dispatch…") passes the same option. Round 2 keeps it
a daemon `--replay` query/env parameter.

## Verification

Captured headless with Playwright (`page.screenshot` samples; frame mtimes are
the timeline) and encoded to 30fps CFR via ffmpeg concat — the proven pipeline
(`recordVideo` truncates and CDP screencast starves on macOS; do not retry).
Deliverables for the bundled arc: `ao-replay-demo-bela45-1x.mp4` (canonical)
and a 2× working cut, in the session artifacts directory.
