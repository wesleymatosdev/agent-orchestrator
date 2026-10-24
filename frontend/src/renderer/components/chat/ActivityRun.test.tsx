import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
import type { ConversationActivity } from "../../types/conversation";
import { ActivityRun } from "./ActivityRun";

function tool(
	id: string,
	providerItemId: string,
	summary: string,
	parentProviderItemId?: string,
): ConversationActivity {
	return {
		kind: "activity",
		id,
		providerItemId,
		sequence: Number(id.replace(/\D/g, "")),
		revision: 0,
		activityKind: "mcp_tool",
		status: "completed",
		summary,
		detail: { parentProviderItemId },
		createdAt: "2026-08-04T00:00:00Z",
	};
}

describe("ActivityRun nested agents", () => {
	it("keeps child-agent work under its parent and collapsed by default", async () => {
		const user = userEvent.setup();
		render(
			<ActivityRun
				activities={[
					tool("activity-1", "agent-tool", "Delegate repository scan"),
					tool("activity-2", "child-tool", "Search source files", "agent-tool"),
				]}
			/>,
		);

		await user.click(screen.getByRole("button", { name: /Ran 2 tool calls/i }));
		expect(await screen.findByRole("button", { name: /Subagent 1 step/i })).toHaveAttribute(
			"aria-expanded",
			"false",
		);
		expect(screen.queryByText("Search source files")).not.toBeInTheDocument();

		await user.click(screen.getByRole("button", { name: /Subagent 1 step/i }));
		expect(await screen.findByText("Search source files")).toBeInTheDocument();
	});

	it("keeps malformed provider cycles visible at the top level", async () => {
		const user = userEvent.setup();
		render(
			<ActivityRun
				activities={[
					tool("activity-1", "tool-a", "First", "tool-b"),
					tool("activity-2", "tool-b", "Second", "tool-a"),
				]}
			/>,
		);
		await user.click(screen.getByRole("button", { name: /Ran 2 tool calls/i }));
		expect(await screen.findByText("First")).toBeInTheDocument();
		expect(await screen.findByText("Second")).toBeInTheDocument();
	});
});

describe("ActivityRun in a running turn", () => {
	const calls = [
		tool("activity-1", "tool-a", "Read package.json"),
		tool("activity-2", "tool-b", "Search for SEC-164"),
	];

	it("shows each call while the turn runs and folds once it settles", async () => {
		const { rerender } = render(<ActivityRun activities={calls} live />);
		expect(screen.getByRole("button", { name: /Ran 2 tool calls/i })).toHaveAttribute("aria-expanded", "true");
		expect(screen.getByText("Read package.json")).toBeInTheDocument();
		expect(screen.getByText("Search for SEC-164")).toBeInTheDocument();

		rerender(<ActivityRun activities={calls} live={false} />);
		expect(screen.getByRole("button", { name: /Ran 2 tool calls/i })).toHaveAttribute("aria-expanded", "false");
		await waitFor(() => expect(screen.queryByText("Read package.json")).not.toBeInTheDocument());
	});

	it("keeps the reader's choice over the live default", async () => {
		const user = userEvent.setup();
		const { rerender } = render(<ActivityRun activities={calls} live />);
		await user.click(screen.getByRole("button", { name: /Ran 2 tool calls/i }));
		await waitFor(() => expect(screen.queryByText("Read package.json")).not.toBeInTheDocument());

		rerender(<ActivityRun activities={calls} live={false} />);
		await user.click(screen.getByRole("button", { name: /Ran 2 tool calls/i }));
		rerender(<ActivityRun activities={calls} live />);
		rerender(<ActivityRun activities={calls} live={false} />);
		expect(screen.getByText("Read package.json")).toBeInTheDocument();
	});

	it("shows a working subagent's steps while the turn runs", () => {
		render(
			<ActivityRun
				live
				activities={[
					tool("activity-1", "agent-tool", "Delegate repository scan"),
					{ ...tool("activity-2", "child-tool", "Search source files", "agent-tool"), status: "running" },
				]}
			/>,
		);
		expect(screen.getByRole("button", { name: /Subagent 1 step/i })).toHaveAttribute("aria-expanded", "true");
		expect(screen.getByText("Search source files")).toBeInTheDocument();
	});
});

/**
 * ActivityRun is controlled when it gets a disclosureKey: opening and closing
 * only ever calls back up. The harness owns that state, like ChatWorkspace does.
 */
function RunHarness({ activities, live }: { activities: ConversationActivity[]; live?: boolean }) {
	const [overrides, setOverrides] = useState<Record<string, boolean>>({});
	return (
		<ActivityRun
			activities={activities}
			live={live}
			disclosureKey="turn-1:run-1"
			disclosureOverrides={overrides}
			onDisclosureChange={(key, open) => setOverrides((prev) => ({ ...prev, [key]: open }))}
		/>
	);
}

describe("ActivityRun keyboard collapse", () => {
	it("collapses an expanded run on Escape without touching later toggles", async () => {
		render(
			<RunHarness
				live
				activities={[
					tool("activity-1", "tool-a", "Read package.json"),
					tool("activity-2", "tool-b", "Search for SEC-164"),
				]}
			/>,
		);
		const toggle = screen.getByRole("button", { name: /Ran 2 tool calls/i });
		expect(toggle).toHaveAttribute("aria-expanded", "true");

		fireEvent.keyDown(toggle, { key: "Escape" });
		expect(toggle).toHaveAttribute("aria-expanded", "false");
		await waitFor(() => expect(screen.queryByText("Read package.json")).not.toBeInTheDocument());
	});

	it("collapses an expanded subgroup on Escape", async () => {
		// Two git commands plus an unrelated tool: more than one group, so the
		// command pair renders behind a real subgroup toggle.
		const commands = [
			{ ...tool("activity-1", "tool-a", "git status"), activityKind: "command" as const, detail: { command: "git status" } },
			{ ...tool("activity-2", "tool-b", "git log"), activityKind: "command" as const, detail: { command: "git log" } },
			tool("activity-3", "tool-c", "Delegate repository scan"),
		];
		render(<RunHarness activities={commands} />);
		await userEvent.click(screen.getByRole("button", { name: /Ran 3 tool calls/i }));

		const subgroup = document.querySelector("button.activity-subgroup-toggle");
		expect(subgroup).not.toBeNull();
		fireEvent.click(subgroup!);
		expect(subgroup).toHaveAttribute("aria-expanded", "true");

		fireEvent.keyDown(subgroup!, { key: "Escape" });
		expect(subgroup).toHaveAttribute("aria-expanded", "false");
	});
});
