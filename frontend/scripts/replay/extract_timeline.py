#!/usr/bin/env python3
"""Build replay fixtures for the AO session-replay demo harness (round 1).

Reads ao.db STRICTLY READ-ONLY and captures of the real daemon API, and writes
the fixture bundle the dev replay server plays back:

  <out>/meta.json                      ids, labels, story window
  <out>/timeline.json                  clock-driven SSE beats (conv, sequence, t)
  <out>/orch-conversation.json         trimmed project-orchestrator snapshot
  <out>/worker-conversation.json       trimmed worker snapshot (no reasoning)
  <out>/worker-session-timeline.json   board field beats for the worker card

Usage:
  extract_timeline.py \
    --project-conv <id> --worker-conv <id> --session <worker-session-id> \
    --start "2026-11-01 14:09:30" --end "2026-11-01 15:40:00" \
    --capture-dir <payloads dir> --out <fixtures dir>

The daemon capture dir must hold the real API responses recorded with plain
GETs (projects.json, sessions.json, orch-session.json, worker-session.json,
orch-conversation.json, worker-conversation.json) so playback serves the exact
wire shapes the daemon produces.
"""

from __future__ import annotations

import argparse
import json
import os
import sqlite3
from datetime import datetime, timedelta, timezone

DEFAULT_DB = os.path.expanduser("~/.ao/data/ao.db")
REASONING_KIND = "reasoning"
LEAD_S = 30.0  # story lead-in before the first event


def parse_ts(s: str) -> float | None:
    """Parse both daemon timestamp shapes: with and without fractional seconds."""
    if not s:
        return None
    s = s.replace(" UTC", "").strip()
    # Some rows carry a trailing "+0000" offset (whole-second timestamps).
    if " +" in s and s[-5] == "+" and s[-4:].isdigit():
        s = s[: -5].strip()
    base, _, rest = s.partition(".")
    frac = rest.split()[0] if rest else ""
    try:
        dt = datetime.strptime(base, "%Y-%m-%d %H:%M:%S")
    except ValueError:
        return None
    if frac:
        dt = dt.replace(microsecond=int(float("0." + frac) * 1e6))
    return dt.replace(tzinfo=timezone.utc).timestamp()


def parse_iso(s: str) -> float | None:
    if not s:
        return None
    try:
        return datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()
    except ValueError:
        return None


def iso(epoch: float) -> str:
    return datetime.fromtimestamp(epoch, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", default=DEFAULT_DB)
    ap.add_argument("--project-conv", required=True)
    ap.add_argument("--worker-conv", required=True)
    ap.add_argument("--session", required=True, help="worker session id (own session-scope conversation)")
    ap.add_argument("--start", required=True, help="story window start, UTC 'YYYY-MM-DD HH:MM:SS'")
    ap.add_argument("--end", required=True, help="story window end, UTC 'YYYY-MM-DD HH:MM:SS'")
    ap.add_argument("--capture-dir", required=True)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    t0 = datetime.strptime(args.start, "%Y-%m-%d %H:%M:%S").replace(tzinfo=timezone.utc).timestamp()
    t_end = datetime.strptime(args.end, "%Y-%m-%d %H:%M:%S").replace(tzinfo=timezone.utc).timestamp()
    if t_end <= t0:
        raise SystemExit("--end must be after --start")

    con = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    cur = con.cursor()

    # Session facts.
    sess = cur.execute(
        "SELECT display_name, kind, project_id, branch, harness FROM sessions WHERE id = ?",
        (args.session,),
    ).fetchone()
    if not sess:
        raise SystemExit(f"session {args.session} not found")
    display_name, kind, project_id, branch, harness = sess
    orch_row = cur.execute(
        "SELECT id FROM sessions WHERE project_id = ? AND kind = 'orchestrator' "
        "ORDER BY created_at DESC LIMIT 1",
        (project_id,),
    ).fetchone()
    if not orch_row:
        raise SystemExit("no orchestrator session for project")
    orch_session = orch_row[0]
    proj = cur.execute("SELECT display_name, path FROM projects WHERE id = ?", (project_id,)).fetchone()

    # Message/activity beats for both conversations, windowed.
    events: list[dict] = []
    conv_items = {"orch": [], "worker": []}
    for conv_key, conv_id in (("orch", args.project_conv), ("worker", args.worker_conv)):
        for seq, role, origin, text, created_at in cur.execute(
            "SELECT sequence, role, origin, text, created_at FROM conversation_messages "
            "WHERE conversation_id = ? ORDER BY sequence",
            (conv_id,),
        ):
            t = parse_ts(created_at)
            if t is None or not text or not (t0 - LEAD_S <= t <= t_end):
                continue
            events.append({"conv": conv_key, "sequence": seq, "kind": "message", "role": role, "t": round(t - t0, 3)})
            conv_items[conv_key].append(("message", seq, t))
        for seq, akind, summary, created_at in cur.execute(
            "SELECT sequence, kind, summary, created_at FROM conversation_activities "
            "WHERE conversation_id = ? AND kind != ? ORDER BY sequence",
            (conv_id, REASONING_KIND),
        ):
            t = parse_ts(created_at)
            if t is None or not (t0 - LEAD_S <= t <= t_end):
                continue
            label = summary or akind
            events.append(
                {"conv": conv_key, "sequence": seq, "kind": "activity", "activityKind": akind, "label": label[:90], "t": round(t - t0, 3)}
            )
            conv_items[conv_key].append(("activity", seq, t))

    events.sort(key=lambda e: e["t"])

    def load(name: str) -> dict:
        with open(os.path.join(args.capture_dir, name)) as f:
            return json.load(f)

    # Trim captured conversation snapshots to the window and drop reasoning.
    def trim_conversation(capture: dict, conv_key: str) -> dict:
        keep = {(kind_, seq) for kind_, seq, _ in conv_items[conv_key]}
        max_seq = max((seq for _, seq, _ in conv_items[conv_key]), default=0)
        min_seq = min((seq for _, seq, _ in conv_items[conv_key]), default=0)
        out = dict(capture)
        out["messages"] = [m for m in capture.get("messages", []) if ("message", m["sequence"]) in keep]
        out["activities"] = [a for a in capture.get("activities", []) if a.get("activityKind") != REASONING_KIND and ("activity", a["sequence"]) in keep]
        out["turns"] = [
            tr for tr in capture.get("turns", [])
            if (parse_iso(tr.get("requestedAt")) or 0) <= t_end
        ]
        out["latestSequence"] = max_seq
        out["oldestSequence"] = min_seq
        out["hasMoreBefore"] = False
        return out

    os.makedirs(args.out, exist_ok=True)
    orch_conv = trim_conversation(load("orch-conversation.json"), "orch")
    worker_conv = trim_conversation(load("worker-conversation.json"), "worker")
    # The project conversation is served under the orchestrator session id.
    orch_conv["sessionId"] = orch_session
    with open(os.path.join(args.out, "orch-conversation.json"), "w") as f:
        json.dump(orch_conv, f, ensure_ascii=False)
    with open(os.path.join(args.out, "worker-conversation.json"), "w") as f:
        json.dump(worker_conv, f, ensure_ascii=False)

    # Worker card field beats, from the captured session row + turn times.
    worker = load("worker-session.json")["session"]
    prs = cur.execute(
        "SELECT url, number, pr_state, ci_state, review_decision, mergeability, review_partial, created_at_provider "
        "FROM pr WHERE session_id = ? ORDER BY created_at_provider",
        (args.session,),
    ).fetchall()
    pr_open = min(
        (t for row in prs if row[7] and (t := parse_ts(row[7].replace(" UTC", "").strip())) is not None),
        default=None,
    )
    turn_times = sorted(parse_iso(tr.get("requestedAt")) or 0 for tr in worker_conv["turns"])
    first_busy = min((t for t in turn_times if t0 <= t <= t_end), default=None)
    last_busy = None
    for tr in worker_conv["turns"]:
        comp = parse_iso(tr.get("completedAt"))
        if comp and comp <= t_end and (last_busy is None or comp > last_busy):
            last_busy = comp
    timeline_fields = {
        "session": {
            "id": worker["id"],
            "title": display_name or worker.get("displayName") or worker["id"],
            "kind": kind or "worker",
            "provider": harness or "hermes",
            "branch": branch or "",
            "createdAt": iso(max(parse_iso(worker.get("createdAt")) or t0, t0)),
            "prOpenAt": iso(pr_open) if pr_open and pr_open <= t_end else None,
            "busyFrom": iso(first_busy) if first_busy else None,
            "busyUntil": iso(last_busy) if last_busy else None,
            "prs": [
                {
                    "url": row[0],
                    "number": row[1],
                    "state": row[2],
                    "ci": row[3],
                    "review": row[4],
                    "mergeability": row[5],
                    "reviewComments": bool(row[6]),
                    "updatedAt": iso(pr_created) if row[7] else None,
                }
                for row in prs
                if row[7]
                and (pr_created := parse_ts(row[7].replace(" UTC", "").strip())) is not None
                and pr_created <= t_end
            ],
        },
        "project": {
            "id": project_id,
            "name": (proj[0] if proj else project_id),
            "path": (proj[1] if proj else ""),
        },
    }
    with open(os.path.join(args.out, "worker-session-timeline.json"), "w") as f:
        json.dump(timeline_fields, f, indent=1)

    meta = {
        "project": timeline_fields["project"],
        "orchestratorSession": orch_session,
        "workerSession": args.session,
        "workerTitle": display_name,
        "branch": branch,
        "t0": iso(t0),
        "tEnd": iso(t_end),
        "durationS": round(t_end - t0),
        "counts": {
            "orch": sum(1 for e in events if e["conv"] == "orch"),
            "worker": sum(1 for e in events if e["conv"] == "worker"),
        },
    }
    with open(os.path.join(args.out, "meta.json"), "w") as f:
        json.dump(meta, f, indent=1)
    with open(os.path.join(args.out, "timeline.json"), "w") as f:
        json.dump({"t0": t0, "tEnd": t_end, "events": events}, f, ensure_ascii=False)

    print(f"wrote {args.out}: {len(events)} beats over {meta['durationS']}s story window")
    print(f"  orchestrator beats: {meta['counts']['orch']}, worker beats: {meta['counts']['worker']}")
    print(f"  worker PR open: {timeline_fields['session']['prOpenAt']}")


if __name__ == "__main__":
    main()
