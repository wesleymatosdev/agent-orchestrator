import { expect, test, type Page } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";

// Visual proof for PR #12: generic command rows in the chat timeline used to all
// read "Ran command", which told the reader nothing. splitSummary() now labels a
// non-read/search/vcs command with the real command — binary plus args — while
// read/search/vcs keep their intent labels. This spec seeds one turn of realistic
// command activity, asserts the rendered labels, and captures the timeline.
const sessionId = "chat-tool-call-visibility";
const now = "2026-08-24T12:00:00Z";

type Seed = {
	id: string;
	command: string;
	status: "completed" | "failed";
	exitCode?: number;
	output?: string;
};

const seeds: Seed[] = [
	{
		id: "act-test",
		command: "go test ./internal/...",
		status: "completed",
		output: "ok  \tagent-orchestrator/internal/service\t0.412s",
	},
	{
		id: "act-build",
		command: "npm run build",
		status: "completed",
		output: "> ao@0.10.0 build\n> vite build",
	},
	// vcs keeps its intent label ("Checked repository") — the command itself is
	// still one click away in the expanded body.
	{ id: "act-vcs", command: "git status --short", status: "completed" },
	// search keeps its intent label ("Search").
	{ id: "act-search", command: 'rg "splitSummary" frontend/src', status: "completed" },
	// Failed styling is unchanged: the label is the command, the state shows exit.
	{
		id: "act-fail",
		command: "cargo build --release",
		status: "failed",
		exitCode: 101,
		output: "error: could not compile `ao` (bin \"ao\")",
	},
];

function snapshot() {
	return {
		conversationId: `conversation-${sessionId}`,
		sessionId,
		harness: "codex",
		mode: "chat",
		controller: "ready",
		latestSequence: 1 + seeds.length,
		oldestSequence: 1,
		hasMoreBefore: false,
		turns: [
			{
				id: "turn-1",
				state: "completed",
				requestedAt: now,
				startedAt: now,
				completedAt: now,
			},
		],
		messages: [
			{
				kind: "message",
				id: "user-1",
				turnId: "turn-1",
				sequence: 1,
				revision: 0,
				role: "user",
				origin: "human",
				text: "Run the checks and build the app",
				streaming: false,
				createdAt: now,
			},
		],
		activities: seeds.map((seed, index) => ({
			kind: "activity",
			id: seed.id,
			turnId: "turn-1",
			// `summary` carries the old generic label on purpose: the row must take
			// its text from detail.command via splitSummary, not from the summary.
			summary: "Ran command",
			sequence: 2 + index,
			revision: 0,
			activityKind: "command",
			status: seed.status,
			detail: {
				command: seed.command,
				...(seed.exitCode !== undefined ? { exitCode: seed.exitCode } : {}),
				...(seed.output ? { output: seed.output } : {}),
			},
			createdAt: now,
		})),
		settings: {},
	};
}

async function installConversation(page: Page) {
	await installFakeAgent(page, {
		workers: [{ id: sessionId, title: "Tool-call visibility", mode: "chat" }],
	});
	await page.route(`**/api/v1/sessions/${sessionId}/**`, async (route) => {
		const path = new URL(route.request().url()).pathname;
		if (path.endsWith("/conversation") && route.request().method() === "GET") {
			await route.fulfill({ json: snapshot() });
			return;
		}
		if (path.endsWith("/conversation/models")) {
			await route.fulfill({ json: { models: [], selected: {} } });
			return;
		}
		if (path.endsWith("/conversation/skills")) {
			await route.fulfill({ json: { skills: [] } });
			return;
		}
		if (path.endsWith("/workspace/files")) {
			await route.fulfill({ json: { files: [], truncated: false } });
			return;
		}
		if (path.endsWith("/interface-transition")) {
			await route.fulfill({ json: { supported: true, targetMode: "tui" } });
			return;
		}
		await route.fulfill({ status: 404, json: { error: { code: "NOT_FOUND", message: "not found" } } });
	});
	await page.goto(`/#/projects/fake-proj/sessions/${sessionId}`);
	await expect(page.getByRole("log", { name: "Conversation" })).toBeVisible();
}

test("command rows show the real command in the chat timeline @T0", async ({ page }) => {
	await installConversation(page);

	// A settled turn folds its work into the "Worked for" accordion; the commands
	// inside group into a collapsed run whose plain-command subgroup nests one
	// more toggle. Open everything so the individual tool-call rows render —
	// that is where the fix shows.
	const log = page.getByRole("log", { name: "Conversation" });
	await log.getByRole("button", { name: /Worked for/ }).click();
	for (const toggle of [".activity-run-toggle", ".activity-subgroup-toggle"]) {
		for (let pass = 0; pass < 5; pass += 1) {
			const collapsed = log.locator(`${toggle}[aria-expanded='false']`);
			if ((await collapsed.count()) === 0) break;
			await collapsed.first().click();
		}
	}

	await expect(log.getByText("go test ./internal/...", { exact: true })).toBeVisible();
	await expect(log.getByText("npm run build", { exact: true })).toBeVisible();
	// Read/search/vcs keep their intent labels.
	await expect(log.getByText("Checked repository", { exact: true })).toBeVisible();
	await expect(log.getByText("Search", { exact: true })).toBeVisible();
	// The failed row keeps its command label and its unchanged failed styling.
	await expect(log.getByText("cargo build --release", { exact: true })).toBeVisible();
	await expect(log.getByText("exit 101", { exact: true })).toBeVisible();
	// The old generic label is gone from the timeline entirely.
	await expect(log.getByText("Ran command", { exact: true })).toHaveCount(0);

	const directory = process.env.AO_TOOL_CALL_EVIDENCE_DIR;
	if (directory) {
		// The accordion and run expansions animate height/opacity; let them finish
		// so the capture shows every settled row instead of a mid-transition clip.
		await page.waitForTimeout(700);
		await page.screenshot({ path: `${directory}/chat-tool-call-visibility.png`, fullPage: true });
	}
});
