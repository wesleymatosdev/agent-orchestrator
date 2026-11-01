/**
 * Dev-only AO session-replay harness (round 1): serves a recorded session
 * timeline to the REAL renderer build through the same HTTP seam the dev proxy
 * uses for the daemon.
 *
 * What runs here is `vite` with the standard renderer config plus one plugin
 * (`aoReplayServer`) whose middleware owns every `/api/v1/*` request the app
 * makes, answering from fixtures extracted from ao.db + real daemon captures
 * (see extract_timeline.py). A story clock (wall time x scale) drives:
 *
 *   - GET /api/v1/sessions/{id}/conversation  time-sliced snapshot
 *   - GET /api/v1/sessions/{id}               time-sliced session detail
 *   - GET /api/v1/events                      SSE CDC frames at beat times
 *   - GET /__ao_replay/board                  board snapshot for the documented
 *                                             `window.__aoFakeAgent` preview seam
 *
 * Zero app components are edited: the renderer loads through its normal
 * index.html (the plugin only injects this replay's boot script), talks to its
 * normal api client, and renders its normal board/chat components. The round-2
 * `ao replay` daemon endpoint replaces this middleware one route at a time by
 * serving the same paths from the daemon process.
 *
 * Usage:
 *   node scripts/replay/replay-server.mjs [--fixtures dir] [--port 4173] [--scale 12]
 * then open http://127.0.0.1:<port>/projects/<project>/sessions/<orchestrator-session>
 */

import { readFileSync, existsSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const frontendDir = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
// Scripts live under scripts/replay/, outside vite's resolution root — import
// the frontend-local vite explicitly.
const viteModule = await import(pathToFileURL(resolve(frontendDir, "node_modules/vite/dist/node/index.js")).href);
const createServer = viteModule.createServer;

function parseArgs(argv) {
	const out = {
		fixtures: resolve(frontendDir, "scripts/replay/fixtures"),
		port: 4173,
		scale: 12,
		// Whether command/file-change/mcp_tool activity rows render in the
		// replay. Default SHOWN (owner preference); REPLAY_SHOW_COMMANDS=0 or
		// --show-commands 0 hides them. The round-3 UI control belongs to the
		// export-button spec (see README.md).
		showCommands: process.env.REPLAY_SHOW_COMMANDS !== "0",
	};
	for (let i = 2; i < argv.length; i += 1) {
		const flag = argv[i];
		const next = () => argv[++i];
		if (flag === "--fixtures") out.fixtures = resolve(next());
		else if (flag === "--port") out.port = Number(next());
		else if (flag === "--scale") out.scale = Number(next());
		else if (flag === "--show-commands") out.showCommands = next() !== "0";
		else throw new Error(`unknown flag: ${flag}`);
	}
	return out;
}

function loadFixtures(dir) {
	const read = (name) => JSON.parse(readFileSync(resolve(dir, name), "utf8"));
	const fixtures = {
		meta: read("meta.json"),
		timeline: read("timeline.json"),
		workerFields: read("worker-session-timeline.json"),
		orchConversation: read("orch-conversation.json"),
		workerConversation: read("worker-conversation.json"),
	};
	// Optional captures that keep peripheral UI quiet; absence is fine — the
	// middleware 404s and logs them so gaps are visible.
	for (const [key, name] of [
		["projects", "projects.json"],
		["orchSession", "orch-session.json"],
		["workerSession", "worker-session.json"],
	]) {
		const path = resolve(dir, name);
		if (existsSync(path)) fixtures[key] = JSON.parse(readFileSync(path, "utf8"));
	}
	return fixtures;
}

/** Replay state shared by the middleware and the SSE driver. */
function createReplay(opts, fixtures) {
	const startedAtMs = Date.now();
	const t0 = fixtures.timeline.t0;
	const tEnd = fixtures.timeline.tEnd;
	const durationS = tEnd - t0;
	const meta = fixtures.meta;
	const orchId = meta.orchestratorSession;
	const workerId = meta.workerSession;
	const project = meta.project;
	const fields = fixtures.workerFields.session;

	// Activity rows hidden by the show-commands option. Filtering applies to
	// the SSE schedule and the served activity lists, but never to the
	// sequence cutoff: message visibility must keep its real timing.
	const hiddenActivityKinds = opts.showCommands
		? new Set()
		: new Set(["command", "file_change", "mcp_tool"]);

	const storyOf = (isoString) => {
		const t = Date.parse(isoString);
		return Number.isNaN(t) ? null : t / 1000 - t0;
	};

	// Piecewise time warps: the story clock runs at different scales per
	// segment — slow through the demo scenes (dispatch, board, card open, PR
	// open), fast across quiet work stretches. Every consumer (snapshot
	// slicing, SSE schedule, boot clock) integrates the same segments, so all
	// views agree on "now".
	const spawnStory = storyOf(fields.createdAt) ?? 0;
	const prStory = fields.prOpenAt ? storyOf(fields.prOpenAt) : null;
	const warps = [
		{ from: 0, to: spawnStory + 40, scale: 8 }, // orchestrator dispatch scene
		{ from: spawnStory + 40, to: spawnStory + 150, scale: 5 }, // board + card open + task brief
		{ from: spawnStory + 150, to: spawnStory + 700, scale: 30 }, // worker early work
	];
	if (prStory !== null && prStory > spawnStory + 700) {
		warps.push({ from: spawnStory + 700, to: prStory - 120, scale: 80 });
		warps.push({ from: prStory - 120, to: prStory + 180, scale: 10 }); // PR open beat
		warps.push({ from: prStory + 180, to: durationS + 300, scale: 80 });
	} else {
		warps.push({ from: spawnStory + 700, to: durationS + 300, scale: 80 });
	}
	const storyNow = () => {
		const wallS = (Date.now() - startedAtMs) / 1000;
		let wallUsed = 0;
		for (const warp of warps) {
			const segWall = (warp.to - warp.from) / warp.scale;
			if (wallS < wallUsed + segWall) return warp.from + (wallS - wallUsed) * warp.scale;
			wallUsed += segWall;
		}
		return Math.min(durationS + 300, warps[warps.length - 1].to);
	};
	// Inverse: wall seconds at which a story offset fires (SSE scheduling).
	const wallOfStory = (storyS) => {
		let wallAcc = 0;
		for (const warp of warps) {
			if (storyS <= warp.to) return wallAcc + (storyS - warp.from) / warp.scale;
			wallAcc += (warp.to - warp.from) / warp.scale;
		}
		return wallAcc;
	};
	// Rewrite a story offset (seconds since the window start) to the replay's
	// wall clock so relative labels ("3m ago") read exactly like a live session.
	// Uses the warp segment containing the offset, matching storyNow().
	const wallIso = (storyOffsetS) => {
		for (const warp of warps) {
			if (storyOffsetS <= warp.to) {
				const warpWallStart = warps
					.slice(0, warps.indexOf(warp))
					.reduce((acc, w) => acc + (w.to - w.from) / w.scale, 0);
				return new Date(
					startedAtMs + (warpWallStart + (storyOffsetS - warp.from) / warp.scale) * 1000,
				).toISOString();
			}
		}
		return new Date().toISOString();
	};
	// Story offset (seconds since the window start) of a recorded timestamp.

	// Max visible sequence per conversation at story time.
	const cutoffByConv = { orch: 0, worker: 0 };
	const convSession = { orch: orchId, worker: workerId };
	const convId = { orch: fixtures.orchConversation.conversationId, worker: fixtures.workerConversation.conversationId };
	for (const event of fixtures.timeline.events) {
		cutoffByConv[event.conv] = Math.max(cutoffByConv[event.conv], event.sequence);
	}
	const cutoffAt = (conv, storyT) => {
		let cutoff = 0;
		for (const event of fixtures.timeline.events) {
			if (event.conv === conv && event.t <= storyT) cutoff = Math.max(cutoff, event.sequence);
		}
		return cutoff;
	};

	function sliceConversation(conv, storyT) {
		const capture = conv === "orch" ? fixtures.orchConversation : fixtures.workerConversation;
		const cutoff = cutoffAt(conv, storyT);
		const inWindow = (items) => items.filter((item) => item.sequence <= cutoff && item.sequence >= 1);
		const messages = inWindow(capture.messages ?? []);
		const activities = inWindow(capture.activities ?? []).filter(
			(activity) => !hiddenActivityKinds.has(activity.activityKind),
		);
		const turns = (capture.turns ?? []).flatMap((turn) => {
			const reqT = storyOf(turn.requestedAt);
			if (reqT === null || reqT > storyT) return [];
			const doneT = turn.completedAt ? storyOf(turn.completedAt) : null;
			const running = doneT === null || doneT > storyT;
			const sliced = { ...turn };
			if (running) {
				sliced.state = "running";
				sliced.completedAt = null;
				sliced.errorMessage = undefined;
			}
			sliced.requestedAt = wallIso(reqT);
			if (turn.startedAt) sliced.startedAt = wallIso(Math.max(turn.startedAt ? storyOf(turn.startedAt) ?? reqT : reqT, reqT));
			if (sliced.completedAt && !running) sliced.completedAt = wallIso(doneT);
			return [sliced];
		});
		const busy = turns.some((turn) => turn.state === "running");
		return {
			...capture,
			messages,
			activities,
			turns,
			controller: busy ? "busy" : "ready",
			latestSequence: cutoff,
			oldestSequence: messages.length > 0 || activities.length > 0 ? Math.min(...[...messages, ...activities].map((item) => item.sequence)) : cutoff,
			hasMoreBefore: false,
		};
	}

	function sessionDetail(sessionId, storyT) {
		if (sessionId === orchId) {
			return (
				fixtures.orchSession?.session ?? {
					id: orchId,
					projectId: project.id,
					kind: "orchestrator",
					displayName: "",
					mode: "chat",
					activity: { state: "active", lastActivityAt: new Date().toISOString() },
					isTerminated: false,
					provisionState: "ready",
					createdAt: new Date().toISOString(),
					updatedAt: new Date().toISOString(),
					prs: [],
				}
			);
		}
		if (sessionId === workerId && fixtures.workerSession) {
			const base = { ...fixtures.workerSession.session };
			const createdT = storyOf(fields.createdAt) ?? 0;
			const busyFromT = fields.busyFrom ? storyOf(fields.busyFrom) : null;
			const busyUntilT = fields.busyUntil ? storyOf(fields.busyUntil) : null;
			const prOpenT = fields.prOpenAt ? storyOf(fields.prOpenAt) : null;
			const busy = busyFromT !== null && busyFromT <= storyT && (busyUntilT === null || storyT < busyUntilT);
			base.activity = {
				state: busy ? "active" : "idle",
				lastActivityAt: new Date().toISOString(),
			};
			base.createdAt = wallIso(Math.max(createdT, t0));
			base.updatedAt = new Date().toISOString();
			base.isTerminated = false;
			base.displayStatus = undefined;
			base.status = prOpenT !== null && prOpenT <= storyT ? "review_pending" : "working";
			base.kanbanColumn = prOpenT !== null && prOpenT <= storyT ? "needs_review" : "building";
			base.prs = (prOpenT !== null && prOpenT <= storyT ? fields.prs ?? [] : []).map((pr) => ({
				...pr,
				state: "open",
				updatedAt: wallIso(prOpenT),
			}));
			return base;
		}
		return undefined;
	}

	// Board snapshot mirroring the renderer's WorkspaceSummary shape — the same
	// shape mock-data.ts uses in preview mode.
	function boardSnapshot(storyT) {
		const createdT = storyOf(fields.createdAt) ?? 0;
		const busyFromT = fields.busyFrom ? storyOf(fields.busyFrom) : null;
		const busyUntilT = fields.busyUntil ? storyOf(fields.busyUntil) : null;
		const prOpenT = fields.prOpenAt ? storyOf(fields.prOpenAt) : null;
		const nowIso = new Date().toISOString();
		const busy = busyFromT !== null && busyFromT <= storyT && (busyUntilT === null || storyT < busyUntilT);
		const orchestrator = {
			id: orchId,
			workspaceId: project.id,
			workspaceName: project.name,
			title: "Project orchestrator",
			provider: "hermes",
			kind: "orchestrator",
			mode: "chat",
			branch: "main",
			status: "working",
			kanbanColumn: "building",
			displayStatus: "Working",
			createdAt: wallIso(t0),
			updatedAt: nowIso,
			activity: { state: "active", lastActivityAt: nowIso },
			prs: [],
			isTerminated: false,
		};
		const sessions = [orchestrator];
		if (createdT - 30 <= storyT) {
			sessions.push({
				id: workerId,
				workspaceId: project.id,
				workspaceName: project.name,
				title: fields.title,
				provider: fields.provider || "hermes",
				kind: "worker",
				mode: "chat",
				branch: fields.branch,
				status: prOpenT !== null && prOpenT <= storyT ? "review_pending" : "working",
				kanbanColumn: prOpenT !== null && prOpenT <= storyT ? "needs_review" : "building",
				displayStatus: prOpenT !== null && prOpenT <= storyT ? "Needs human review" : "Working",
				createdAt: wallIso(Math.max(createdT, t0)),
				updatedAt: nowIso,
				activity: { state: busy ? "active" : "idle", lastActivityAt: nowIso },
				prs: prOpenT !== null && prOpenT <= storyT ? (fields.prs ?? []).map((pr) => ({ ...pr, state: "open", updatedAt: wallIso(prOpenT) })) : [],
				isTerminated: false,
			});
		}
		return [
			{
				id: project.id,
				name: project.name,
				path: project.path,
				type: "main",
				orchestratorAgent: "hermes",
				accentColor: "var(--color-project-accent-mint)",
				sessions,
			},
		];
	}

	// SSE frames: every conversation beat invalidates that conversation; board
	// beats invalidate the workspace via an id-only session_updated (the client
	// throttles those to one refresh per 5s, so beats are spaced server-side).
	function sseFrames() {
		const frames = [];
		const boardBeats = new Set([0]);
		if (fields.createdAt) boardBeats.add(Math.max(storyOf(fields.createdAt), 0));
		if (fields.prOpenAt) boardBeats.add(Math.max(storyOf(fields.prOpenAt), 0));
		if (fields.busyFrom) boardBeats.add(Math.max(storyOf(fields.busyFrom), 0));
		if (fields.busyUntil) boardBeats.add(Math.max(storyOf(fields.busyUntil), 0));
		for (const event of fixtures.timeline.events) {
			if (hiddenActivityKinds.has(event.kind === "activity" ? event.activityKind : "")) continue;
			const delayS = Math.max(event.t, 0);
			frames.push({
				atS: delayS,
				name: "session_updated",
				data: JSON.stringify({
					type: "session_updated",
					sessionId: convSession[event.conv],
					payload: { conversationId: convId[event.conv] },
				}),
			});
		}
		for (const beat of boardBeats) {
			frames.push({
				atS: beat,
				name: "session_updated",
				data: JSON.stringify({ type: "session_updated", sessionId: workerId, payload: { sessionId: workerId } }),
			});
		}
		frames.sort((a, b) => a.atS - b.atS);
		// Enforce 6s spacing between id-only board frames so the client's
		// 5s id-only throttle never swallows a lane transition.
		let lastBoard = -Infinity;
		for (const frame of frames) {
			const isBoard = !frame.data.includes("conversationId");
			if (isBoard) {
				if (frame.atS - lastBoard < 6) frame.atS = lastBoard + 6;
				lastBoard = frame.atS;
			}
		}
		return frames;
	}

	return {
		opts, startedAtMs, t0, tEnd, durationS, meta, orchId, workerId, project, warps,
		storyNow, wallOfStory, sliceConversation, sessionDetail, boardSnapshot, sseFrames, cutoffAt,
	};
}

async function main() {
	const opts = parseArgs(process.argv);
	const fixtures = loadFixtures(opts.fixtures);
	const replay = createReplay(opts, fixtures);
	const unmatched = new Set();

	const { VITE_NO_ELECTRON, VITE_AO_API_BASE_URL, ...restEnv } = process.env;
	// Preview mode renders the board via __aoFakeAgent and skips Electron-only
	// gates; the explicit API base marks the daemon URL trusted so the SSE
	// event transport connects to this server.
	process.env.VITE_NO_ELECTRON = "1";
	process.env.VITE_AO_API_BASE_URL = `http://127.0.0.1:${opts.port}`;

	const aoReplayServer = {
		name: "ao-replay-server",
		transformIndexHtml(html) {
			return html.replace(
				"</head>",
				'<script src="/__ao_replay/boot.js"></script>\n  </head>',
			);
		},
		configureServer(server) {
			server.middlewares.use((req, res, next) => {
				const url = new URL(req.url ?? "/", `http://127.0.0.1:${opts.port}`);
				const path = url.pathname;
				const sendJson = (status, body) => {
					res.statusCode = status;
					res.setHeader("Content-Type", "application/json");
					res.end(JSON.stringify(body));
				};

				if (path === "/__ao_replay/boot.js") {
					res.setHeader("Content-Type", "text/javascript");
					res.end(readFileSync(resolve(frontendDir, "scripts/replay/boot.js"), "utf8"));
					return;
				}
				if (path === "/__ao_replay/state") {
					sendJson(200, {
						startedAt: replay.startedAtMs,
						scale: opts.scale,
						warps: replay.warps,
						storyNow: replay.storyNow(),
						durationS: replay.durationS,
						meta: replay.meta,
					});
					return;
				}
				if (path === "/__ao_replay/board") {
					sendJson(200, replay.boardSnapshot(replay.storyNow()));
					return;
				}
				if (path === "/api/v1/events") {
					res.statusCode = 200;
					res.setHeader("Content-Type", "text/event-stream");
					res.setHeader("Cache-Control", "no-cache");
					res.setHeader("Connection", "keep-alive");
					res.write("retry: 2000\n\n");
					const timer = setInterval(() => res.write(`: ping ${Date.now()}\n\n`), 15_000);
					const schedulers = replay
						.sseFrames()
						.map((frame) =>
							setTimeout(() => {
								res.write(`event: ${frame.name}\ndata: ${frame.data}\n\n`);
							}, replay.wallOfStory(frame.atS) * 1000),
						);
					req.on("close", () => {
						clearInterval(timer);
						schedulers.forEach(clearTimeout);
					});
					return;
				}

				const conversationMatch = path.match(/^\/api\/v1\/sessions\/([^/]+)\/conversation$/);
				if (req.method === "GET" && conversationMatch) {
					const sessionId = decodeURIComponent(conversationMatch[1]);
					const conv = sessionId === replay.orchId ? "orch" : sessionId === replay.workerId ? "worker" : undefined;
					if (!conv) return sendJson(404, { error: { message: "replay: unknown session" } });
					sendJson(200, replay.sliceConversation(conv, replay.storyNow()));
					return;
				}
				const staticEndpoint = path.match(/^\/api\/v1\/sessions\/([^/]+)\/(conversation\/config-options|conversation\/models|conversation\/skills|reviews)$/);
				if (req.method === "GET" && staticEndpoint) {
					const sessionId = decodeURIComponent(staticEndpoint[1]);
					const name = staticEndpoint[2].replace("/", "_");
					const file = resolve(frontendDir, `scripts/replay/fixtures/perc/ep-${sessionId}-${name}.json`);
					const fallback = resolve(frontendDir, `scripts/replay/fixtures/perc/ep-${replay.orchId}-${name}.json`);
					const target = existsSync(file) ? file : fallback;
					if (existsSync(target)) {
						res.setHeader("Content-Type", "application/json");
						res.end(readFileSync(target, "utf8"));
						return;
					}
					unmatched.add(`${req.method} ${path}`);
					sendJson(404, { error: { message: `replay: not served (${path})` } });
					return;
				}
				if (req.method === "GET" && path === "/api/v1/notifications") {
					const file = resolve(frontendDir, "scripts/replay/fixtures/perc/ep-notifications.json");
					if (existsSync(file)) {
						res.setHeader("Content-Type", "application/json");
						res.end(readFileSync(file, "utf8"));
						return;
					}
					sendJson(200, { notifications: [], unreadCount: 0, unresolvedCount: 0 });
					return;
				}
				const simplePerc = {
					"/api/v1/settings": "ep-settings.json",
					"/api/v1/mobile/status": "ep-mobile-status.json",
					"/api/v1/awake": "ep-awake.json",
				};
				if (req.method === "GET" && simplePerc[path]) {
					const file = resolve(frontendDir, `scripts/replay/fixtures/perc/${simplePerc[path]}`);
					if (existsSync(file)) {
						res.setHeader("Content-Type", "application/json");
						res.end(readFileSync(file, "utf8"));
						return;
					}
				}
				const agentModels = path.match(/^\/api\/v1\/agents\/([^/]+)\/models$/);
				if (req.method === "GET" && agentModels) {
					const file = resolve(frontendDir, `scripts/replay/fixtures/perc/ep-agents-${decodeURIComponent(agentModels[1])}-models.json`);
					if (existsSync(file)) {
						res.setHeader("Content-Type", "application/json");
						res.end(readFileSync(file, "utf8"));
						return;
					}
				}
				const projectRoute = path.match(/^\/api\/v1\/projects\/([^/]+)$/);
				if (req.method === "GET" && projectRoute) {
					const file = resolve(frontendDir, `scripts/replay/fixtures/perc/ep-projects-${decodeURIComponent(projectRoute[1])}.json`);
					if (existsSync(file)) {
						res.setHeader("Content-Type", "application/json");
						res.end(readFileSync(file, "utf8"));
						return;
					}
				}
				const sessionSub = path.match(/^\/api\/v1\/sessions\/([^/]+)\/(interface-transition|workspace\/files|workspace\/history|workspace\/manifest|pr)$/);
				if (req.method === "GET" && sessionSub) {
					const name =
						sessionSub[2] === "interface-transition" ? "iface"
						: sessionSub[2] === "workspace/files" ? "wsfiles"
						: sessionSub[2] === "workspace/history" ? "history"
						: sessionSub[2] === "workspace/manifest" ? "manifest"
						: "pr";
					const file = resolve(frontendDir, `scripts/replay/fixtures/perc/ep-${name}-${decodeURIComponent(sessionSub[1])}.json`);
					if (existsSync(file)) {
						res.setHeader("Content-Type", "application/json");
						res.end(readFileSync(file, "utf8"));
						return;
					}
					// No capture: interface-transition and pr are quiet when absent;
					// the workspace paths 404 like the real daemon.
					if (name === "iface" || name === "pr") {
						sendJson(200, {});
						return;
					}
				}
				if (req.method === "GET" && (path === "/api/v1/agents/readiness" || path === "/api/v1/agents/chat/unavailable")) {
					const name = path.endsWith("readiness") ? "ep-agents-readiness.json" : "ep-agents-chat-unavailable.json";
					const file = resolve(frontendDir, `scripts/replay/fixtures/perc/${name}`);
					if (existsSync(file)) {
						res.setHeader("Content-Type", "application/json");
						res.end(readFileSync(file, "utf8"));
						return;
					}
				}
				if (req.method === "GET" && path.endsWith("/stream") || req.method === "GET" && path.endsWith("/events")) {
					// Secondary SSE surfaces (notifications, workspace file events):
					// hold an idle open stream so the transports stay quiet.
					res.statusCode = 200;
					res.setHeader("Content-Type", "text/event-stream");
					res.setHeader("Cache-Control", "no-cache");
					res.write("retry: 5000\n\n");
					const timer = setInterval(() => res.write(`: ping ${Date.now()}\n\n`), 25_000);
					req.on("close", () => clearInterval(timer));
					return;
				}
				const sessionMatch = path.match(/^\/api\/v1\/sessions\/([^/]+)$/);
				if (req.method === "GET" && sessionMatch) {
					const detail = replay.sessionDetail(decodeURIComponent(sessionMatch[1]), replay.storyNow());
					if (!detail) return sendJson(404, { error: { message: "replay: unknown session" } });
					sendJson(200, { session: detail });
					return;
				}
				if (req.method === "GET" && path === "/api/v1/projects" && fixtures.projects) {
					sendJson(200, fixtures.projects);
					return;
				}
				if (req.method === "POST" && (path.endsWith("/send") || path.startsWith("/api/v1/"))) {
					// The demo never really mutates; accept and drop any incidental
					// UI write so replay state stays exactly what the clock serves.
					sendJson(200, { accepted: true });
					return;
				}
				if (path.startsWith("/api/v1/") || path.startsWith("/mux")) {
					unmatched.add(`${req.method} ${path}`);
					console.error(`[replay-unmatched] ${req.method} ${path}`);
					sendJson(404, { error: { message: `replay: not served (${path})` } });
					return;
				}
				next();
			});
		},
	};

	const server = await createServer({
		configFile: resolve(frontendDir, "vite.renderer.config.ts"),
		root: frontendDir,
		server: { port: opts.port, strictPort: true, host: "127.0.0.1", proxy: {} },
		plugins: [aoReplayServer],
	});
	await server.listen();

	console.log(`AO replay harness (dev-only)`);
	console.log(`  story window : ${replay.meta.t0} -> ${replay.meta.tEnd} (${replay.durationS}s story)`);
	console.log(`  warps        : ${replay.warps.map((w) => `${Math.round(w.from)}-${Math.round(w.to)}@${w.scale}x`).join("  ")}`);
	console.log(`  wall length  : ~${Math.round(replay.wallOfStory(replay.durationS))}s`);
	console.log(`  activity rows: ${opts.showCommands ? "shown" : "hidden (command/file_change/mcp_tool filtered)"} [REPLAY_SHOW_COMMANDS=0 to hide]`);
	console.log(`  orchestrator : /projects/${replay.project.id}/sessions/${replay.orchId}`);
	console.log(`  board        : /projects/${replay.project.id}`);
	console.log(`  worker       : /projects/${replay.project.id}/sessions/${replay.workerId}`);
	console.log(`  open         : http://127.0.0.1:${opts.port}/projects/${replay.project.id}/sessions/${replay.orchId}`);

	const report = () => {
		if (unmatched.size > 0) console.log(`\nunmatched API paths:\n  ${[...unmatched].sort().join("\n  ")}`);
	};
	process.on("SIGINT", () => {
		report();
		process.exit(0);
	});
	process.on("SIGTERM", () => {
		report();
		process.exit(0);
	});
}

main().catch((error) => {
	console.error(error);
	process.exit(1);
});
