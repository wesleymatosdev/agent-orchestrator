# Visual proof (required for user-visible changes)

Every PR that changes something a user can see ships with evidence, formatted
as a markdown table and — when the change involves interactions — a screencast.
Exemplar: OrchestratorInc/agent-orchestrator#6604.

## Table

```markdown
| | Before (base) | After (this branch) |
|---|---|---|
| <behavior name> | <what happened> | <what happens now> |
```

Each row pairs screenshots (before vs after) hosted as release assets on this
repository (tag pattern: `pr-<number>-images`), referenced by URL so they
render inline.

## Screencast (interaction changes)

If the change involves interactions — clicks, drags, typing, scrolling, any
flow a static pair of screenshots cannot prove — record a screencast by
enabling video in the Playwright proof run (`video: "on"` is set in
`playwright.config.ts`; recordings land next to traces in `.proof/reports/`).
Trim to the interaction, upload with the screenshots, and embed it in the
table row or directly under it.

## Rules

- Before AND after, never only the end result.
- Real renderer, real data (dev:web against the daemon, or the desktop lab) —
  no mocked screenshots presented as app state.
- Static-only changes (pure refactors, CI) are exempt.
