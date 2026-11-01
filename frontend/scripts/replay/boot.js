/**
 * AO replay harness boot script (dev-only). Injected into index.html by the
 * replay server middleware before the renderer bundle loads. It feeds the
 * documented preview board seam (`window.__aoFakeAgent`, consumed by
 * useWorkspaceQuery when VITE_NO_ELECTRON=1) with clock-driven board snapshots,
 * and exposes the story clock for the demo driver.
 *
 * The app's own components render everything: this script only supplies data
 * at the same boundary the round-2 daemon --replay endpoint will own.
 */
(() => {
	const boot = {
		state: null,
		board: [],
		boardFetchedAt: 0,
	};

	// Piecewise time-warp integration — must match the server's storyNow().
	const storyNowFromState = (state) => {
		const wallS = (Date.now() - state.startedAt) / 1000;
		let wallUsed = 0;
		for (const warp of state.warps) {
			const segWall = (warp.to - warp.from) / warp.scale;
			if (wallS < wallUsed + segWall) return warp.from + (wallS - wallUsed) * warp.scale;
			wallUsed += segWall;
		}
		return Math.min(state.durationS + 300, state.warps[state.warps.length - 1].to);
	};

	const refreshBoard = () => {
		fetch("/__ao_replay/board", { headers: { accept: "application/json" } })
			.then((response) => (response.ok ? response.json() : []))
			.then((board) => {
				boot.board = Array.isArray(board) ? board : [];
				boot.boardFetchedAt = Date.now();
			})
			.catch(() => undefined);
	};

	const start = () => {
		fetch("/__ao_replay/state")
			.then((response) => (response.ok ? response.json() : null))
			.then((state) => {
				if (!state) return;
				boot.state = state;
				window.__aoReplay = {
					state,
					storyNow: () => storyNowFromState(state),
				};
				refreshBoard();
				// setInterval, never rAF: rAF pauses in occluded/headless tabs.
				setInterval(refreshBoard, 1000);
			})
			.catch(() => undefined);
	};

	// The e2e fake-agent seam: useWorkspaceQuery calls snapshot() on every
	// preview workspace fetch; SSE-driven invalidations refetch ~instantly.
	window.__aoFakeAgent = {
		snapshot: () => boot.board,
	};

	start();
})();
