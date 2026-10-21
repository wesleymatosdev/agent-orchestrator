import { expect, test, type Page } from "@playwright/test";
import { installFakeAgent } from "./support/fake-bridge";

// Visual proof for the timeline expand/collapse feature. One seeded settled
// turn drives the three flows Wesley reviews: expand a tool-call row in place
// (full command + output), Esc to collapse, and per-turn hide with the
// "N tool calls" count as the collapsed form. Screenshots land in
// AO_TIMELINE_E2C_EVIDENCE_DIR when set; the run's video (playwright.config
// records every test) is the screencast embedded in the PR.
const sessionId = "chat-timeline-expand-collapse";
const now = "2026-08-24T12:00:00Z";

const longCommand =
	"docker buildx build --platform linux/amd64,linux/arm64 --tag registry.internal.example.com/ao/daemon:2026.10 --push .";

const seeds = [
	{
		id: "act-test",
		command: "go test ./internal/...",
		status: "completed",
		output: "ok  \tagent-orchestrator/internal/service\t0.412s",
	},
	{
		id: "act-long",
		command: longCommand,
		status: "completed",
		output: "sha256:9f2c1e… pushed",
	},
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
		latestSequence: 1 + seeds.length + 1,
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
				text: "Run the checks, publish the image, and try the release build",
				streaming: false,
				createdAt: now,
			},
			{
				kind: "message",
				id: "answer-1",
				turnId: "turn-1",
				sequence: seeds.length + 2,
				revision: 0,
				role: "assistant",
				origin: "provider",
				text: "Tests pass and the multi-arch image is up; the release build needs a look.",
				streaming: false,
				createdAt: now,
			},
		],
		activities: seeds.map((seed, index) => ({
			kind: "activity",
			id: seed.id,
			turnId: "turn-1",
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
		workers: [{ id: sessionId, title: "Timeline expand/collapse", mode: "chat" }],
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

async function settleAnimation(page: Page) {
	// Accordions and run toggles animate height/opacity; let them finish so the
	// captures show settled rows instead of mid-transition clips.
	await page.waitForTimeout(700);
}

test("timeline expand, escape, and per-turn hide flows @T0", async ({ page }) => {
	await installConversation(page);
	const log = page.getByRole("log", { name: "Conversation" });
	const directory = process.env.AO_TIMELINE_E2C_EVIDENCE_DIR;

	// BEFORE: the settled turn folded everything into the "Worked for" row.
	if (directory) {
		await settleAnimation(page);
		await log.getByRole("button", { name: "Hide this turn's tool calls" }).hover();
		await page.screenshot({ path: `${directory}/1-before-collapsed.png`, fullPage: true });
	}

	// Expand: accordion, then the run, then every row — full command + output.
	await log.getByRole("button", { name: /Worked for/ }).click();
	for (const toggle of [".activity-run-toggle", ".activity-subgroup-toggle"]) {
		for (let pass = 0; pass < 5; pass += 1) {
			const collapsed = log.locator(`${toggle}[aria-expanded='false']`);
			if ((await collapsed.count()) === 0) break;
			await collapsed.first().click();
		}
	}

	await expect(log.getByText("go test ./internal/...", { exact: true })).toBeVisible();
	// The long command's full text is in the expanded body (wrapped, selectable).
	const longBody = log.locator("pre.cursor-chat-explore-command", { hasText: "docker buildx build" });
	await expect(longBody).toBeVisible();
	await expect(longBody).toContainText("--tag registry.internal.example.com/ao/daemon:2026.10");
	// The failed row kept its styling in both states.
	await expect(log.getByText("exit 101", { exact: true })).toBeVisible();

	if (directory) {
		await settleAnimation(page);
		await page.screenshot({ path: `${directory}/2-after-expanded.png`, fullPage: true });
	}

	// Esc collapses the focused expanded row (keyboard answer to the click).
	const firstRowToggle = log.locator(".activity-row-toggle[aria-expanded='true']").first();
	await firstRowToggle.focus();
	await firstRowToggle.press("Escape");
	await expect(firstRowToggle).toHaveAttribute("aria-expanded", "false");

	// Per-turn hide: the eye-off control on the accordion row.
	await log.getByRole("button", { name: "Hide this turn's tool calls" }).click();
	const hiddenRow = log.getByRole("button", { name: /3 tool calls/ });
	await expect(hiddenRow).toBeVisible();
	await expect(log.getByText("go test ./internal/...", { exact: true })).toHaveCount(0);
	await expect(log.locator(".activity-run-toggle")).toHaveCount(0);
	// The answer is not a tool row; it stays readable.
	await expect(
		log.getByText("Tests pass and the multi-arch image is up; the release build needs a look."),
	).toBeVisible();

	if (directory) {
		await settleAnimation(page);
		await page.screenshot({ path: `${directory}/3-after-hidden.png`, fullPage: true });
	}

	// Re-show from the collapsed form, then confirm persistence is not reset by
	// hiding again (the store owns the choice, keyed by conversation).
	await hiddenRow.click();
	await expect(log.getByRole("button", { name: /Worked for/ })).toBeVisible();
	await expect(log.getByRole("button", { name: "Hide this turn's tool calls" })).toBeVisible();
	await expect(log.locator(".activity-run-toggle")).toHaveCount(1);
});
