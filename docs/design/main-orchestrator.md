# Design: the main orchestrator

Status: agreed direction (Wesley, 2026-10-21). This document pins the model
before implementation.

## Problem

AO sessions are project-bound. A user juggling several projects meets a
project-scoped surface everywhere: each project has its own orchestrator, and
cross-project work has no natural home. The coordinator agent ends up
anchored in one project (say, agent-orchestrator) while doing work in another
(say, the hermes repo) — the work lands under the wrong project and no single
surface shows the user everything.

## Settled decisions

1. **The Hermes kanban board is not the answer.** AO's board is the system of
   record; the fork does not converge on the hermes board. (Settled — do not
   relitigate.)
2. **Project orchestrators stay.** Each project keeps its own orchestrator:
   it owns that project's workers, worktrees, board column, and CI context.
   Project context belongs to orchestrators and workers, never to the user's
   entry point.
3. **A single main orchestrator sits above them.** Project-less (nullable
   `project_id` on `kind = orchestrator`), pinned in the sidebar, the default
   destination of every new conversation. It does not spawn workers directly —
   it talks to project orchestrators, which spawn workers.

## The hierarchy

```
user ⇄ main orchestrator (project-less)
            ⇅ steer / status rollup
   project orchestrator A   project orchestrator B   …
            ⇅                        ⇅
        workers                  workers
```

- **Down (dispatch):** the main orchestrator routes an outcome to a project
  orchestrator as a steered message ("land the auth refactor", "sync
  upstream"). AO already models orchestrator→children sessions
  (`OrchestratorChildrenSection`); the main orchestrator's children are other
  orchestrators.
- **Up (escalation/status):** project orchestrators surface completions,
  blockers, and decisions to the main orchestrator — the rollup the user
  actually reads. Notification wiring (notify-subscribe style) belongs here.
- **Steering is the transport.** This hierarchy is only as good as mid-turn
  steering (the `_session/steering` work). "Interrupt and redirect" must work
  on every edge of the tree: user→main, main→project orchestrator,
  orchestrator→worker.

## What changes

1. **Data model:** `sessions.project_id` becomes nullable for orchestrator
   kind; the main orchestrator is created once per host (idempotent, pinned).
2. **Routing:** messages sent to the main orchestrator that resolve to a
   project outcome are steered into (or spawn) the right project orchestrator;
   the main orchestrator decides, the user never picks a project first.
3. **Board:** the main orchestrator's board view is a rollup — worker
   clusters grouped by project orchestrator. Per-project boards stay as-is.
4. **UI:** the sidebar pins the main orchestrator above projects; new
   conversation defaults to it.

## Non-goals

- No merging of the hermes board or the hermes gateway transport — the fork
  keeps AO as the front door.
- No direct main-orchestrator→worker spawns in v1: everything routes through
  project orchestrators so project context stays enforced.

## Slices

1. Nullable project for orchestrator sessions + one-time main-orchestrator
   creation (data model, small).
2. Main→project-orchestrator routing (steer into child orchestrator sessions;
   depends on the steering extension landing in hermes' ACP adapter).
3. Rollup board view under the main orchestrator.
4. Escalation wiring: project orchestrator → main notifications.
