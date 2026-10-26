import { act, create, type ReactTestRenderer } from "react-test-renderer";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { STANDALONE_PROJECT } from "./projectFilter";

const fixtures = vi.hoisted(() => {
	const agent = (id: string, label: string) => ({ id, label, authStatus: "authorized" as const });
	return {
		theme: new Proxy({}, { get: () => "#000000" }),
		push: vi.fn(),
		replace: vi.fn(),
		spawn: vi.fn(),
		setActiveProject: vi.fn(),
		config: { host: "test" },
		agents: {
			supported: [agent("claude-code", "Claude Code"), agent("hermes", "Hermes")],
			installed: [agent("claude-code", "Claude Code"), agent("hermes", "Hermes")],
			authorized: [agent("claude-code", "Claude Code"), agent("hermes", "Hermes")],
		},
		projectDetail: {},
	};
});

vi.mock("expo-router", async () => {
	const React = await import("react");
	return {
		useLocalSearchParams: () => ({ hostId: "host-a" }),
		useRouter: () => ({ push: fixtures.push, replace: fixtures.replace }),
	};
});
vi.mock("react-native", async () => {
	const React = await import("react");
	const host = (name: string) => ({ children, ...props }: { children?: React.ReactNode }) =>
		React.createElement(name, props, children);
	return {
		InteractionManager: { runAfterInteractions: (cb: () => void) => { cb(); return { cancel: () => {} }; } },
		Platform: { OS: "ios", select: (options: { ios: unknown }) => options.ios },
		Pressable: host("Pressable"),
		StyleSheet: { create: (styles: object) => styles },
		Text: host("Text"),
		View: host("View"),
	};
});
vi.mock("react-native-keyboard-controller", () => ({
	KeyboardStickyView: ({ children }: { children?: React.ReactNode }) => children,
	useKeyboardState: (select: (state: { height: number }) => number) => select({ height: 0 }),
}));
vi.mock("@expo/ui/community/bottom-sheet", async () => {
	const React = await import("react");
	return {
		default: (props: object) => React.createElement("BottomSheet", props),
		BottomSheetView: (props: object) => React.createElement("BottomSheetView", props),
	};
});
vi.mock("expo-document-picker", () => ({ getDocumentAsync: vi.fn() }));
vi.mock("expo-image-picker", () => ({
	launchImageLibraryAsync: vi.fn(),
	UIImagePickerPreferredAssetRepresentationMode: { Compatible: "compatible" },
}));
vi.mock("expo-file-system", () => ({ File: class File {} }));
vi.mock("../lib/icons", async () => {
	const React = await import("react");
	return { Feather: (props: object) => React.createElement("Feather", props) };
});
vi.mock("../lib/api", () => ({
	ApiError: class ApiError extends Error { code = ""; },
	getAgents: vi.fn(async () => fixtures.agents),
	getSettings: vi.fn(async () => ({ defaultSessionMode: "chat", chatHarnesses: ["claude-code", "hermes"] })),
	getProject: vi.fn(async () => fixtures.projectDetail),
	// The standalone scope deliberately drops "proj-only": the degrade path needs
	// a model that is genuinely unavailable on the other side of the toggle.
	getAgentModels: vi.fn(async (_cfg: unknown, agentId: string, projectId?: string) => ({
		agentId,
		selectionMode: "catalog",
		allowCustom: false,
		models: projectId ? [{ id: "m1", label: "Model One" }, { id: "proj-only", label: "Project Only" }] : [{ id: "m1", label: "Model One" }],
		source: "test",
		stale: false,
		fetchedAt: "2026-01-01T00:00:00Z",
	})),
}));
vi.mock("../lib/haptics", () => ({ haptics: { error: vi.fn(), success: vi.fn(), select: vi.fn(), tap: vi.fn() } }));
vi.mock("../lib/hostRoute", () => ({
	connectedSpawnHosts: (hosts: { hostId: string; name: string; connection: string }[]) =>
		hosts.filter((host) => host.connection === "open").map((host) => ({ id: host.hostId, label: host.name })),
	initialSpawnHostId: ({ routeHostId, currentHostId, hosts }: { routeHostId?: string; currentHostId?: string; hosts: { id: string }[] }) => {
		const available = (id?: string) => Boolean(id && hosts.some((host) => host.id === id));
		if (routeHostId) return available(routeHostId) ? routeHostId : null;
		if (available(currentHostId)) return currentHostId;
		return hosts[0]?.id ?? null;
	},
	spawnHostRequestIsCurrent: (generation: number, currentGeneration: number, requestedHostId: string, selectedHostId: string | null) =>
		generation === currentGeneration && requestedHostId === selectedHostId,
	selectSpawnHost: vi.fn(async ({ select }: { select: (id: string) => void }, nextHostId: string) => select(nextHostId)),
}));
vi.mock("../lib/spawn-attachments", () => ({
	MAX_SPAWN_ATTACHMENTS: 6,
	appendSpawnAttachments: (current: unknown[], next: unknown[]) => ({ attachments: [...current, ...next], error: undefined }),
	photoPickerAttachment: vi.fn(),
	readSpawnAttachments: vi.fn(async (current: unknown[]) => ({ attachments: current, error: undefined })),
}));
vi.mock("../lib/spawn-attachment-row", () => ({ SpawnAttachmentRow: () => null }));
vi.mock("../lib/spawn-composer-controls", async () => {
	const React = await import("react");
	// Probe: the sheet's own state (harness, project, model) is what the tests
	// assert on, and it all flows through this component's props.
	return { SpawnComposerControls: (props: object) => React.createElement("SpawnComposerControls", props) };
});
vi.mock("../lib/spawn-prompt-input", () => ({ SpawnPromptInput: () => null }));
vi.mock("../lib/store", () => ({
	useApp: () => ({
		hostStates: [{ hostId: "host-a", name: "Alpha", connection: "open", loading: false, projects: [{ id: "project-1", name: "Project One" }], projectsKnown: true }],
		activeProjectId: "project-1",
		currentHostId: "host-a",
		configForHost: () => fixtures.config,
		switchHost: vi.fn(),
		setActiveProject: fixtures.setActiveProject,
		spawn: fixtures.spawn,
	}),
}));
vi.mock("../lib/voice/useVoiceInput", () => ({
	useVoiceInput: () => ({ state: "idle", mode: "push", pressIn: vi.fn(), pressOut: vi.fn(), error: undefined, partial: "" }),
}));
vi.mock("../lib/ThemeProvider", () => ({
	useTheme: () => fixtures.theme,
	useThemedStyles: (factory: (theme: object) => object) => factory(fixtures.theme),
}));
vi.mock("../lib/ui", async () => {
	const React = await import("react");
	return { Button: (props: object) => React.createElement("Button", props) };
});
vi.mock("../lib/backNavigation", () => ({ backOr: vi.fn() }));
vi.mock("../lib/RouteErrorBoundary", () => ({ SheetErrorBoundary: () => null }));

import SpawnModal from "../app/spawn";

let renderer: ReactTestRenderer | undefined;

beforeEach(() => {
	vi.stubGlobal("IS_REACT_ACT_ENVIRONMENT", true);
	fixtures.spawn.mockReset();
	fixtures.setActiveProject.mockReset();
});

afterEach(async () => {
	if (renderer) await act(async () => renderer?.unmount());
	renderer = undefined;
	vi.unstubAllGlobals();
});

const flush = async () => {
	for (let i = 0; i < 4; i += 1) await act(async () => { await new Promise((resolve) => setTimeout(resolve, 0)); });
};

const controlsOf = (root: ReactTestRenderer["root"]) =>
	root.findAll((node) => String(node.type) === "SpawnComposerControls")[0].props as {
		harness: string;
		projectId: string | null;
		modelSelection: string;
		modelLabel: string;
		onSelectHarness: (id: string) => void;
		onSelectProject: (id: string | null) => void;
		onSelectModel: (id: string) => void;
	};

const openSheet = async () => {
	let instance: ReactTestRenderer | undefined;
	await act(async () => { instance = create(<SpawnModal />); });
	await flush();
	renderer = instance!;
	return controlsOf(renderer.root);
};

const textContents = (root: ReactTestRenderer["root"]) =>
	root.findAll((node) => String(node.type) === "Text").flatMap((node) => {
		const value = node.props.children;
		return typeof value === "string" ? [value] : [];
	});

describe("spawn sheet scope toggle", () => {
	it("keeps a picked harness when the project toggles to standalone", async () => {
		const controls = await openSheet();
		expect(controls.projectId).toBe("project-1");

		await act(async () => controlsOf(renderer!.root).onSelectHarness("hermes"));
		expect(controlsOf(renderer!.root).harness).toBe("hermes");

		await act(async () => controlsOf(renderer!.root).onSelectProject(STANDALONE_PROJECT));
		await flush();

		expect(controlsOf(renderer!.root).projectId).toBe(STANDALONE_PROJECT);
		expect(controlsOf(renderer!.root).harness).toBe("hermes");
	});

	it("keeps a picked harness when standalone toggles back to a project", async () => {
		const controls = await openSheet();

		await act(async () => controlsOf(renderer!.root).onSelectProject(STANDALONE_PROJECT));
		await flush();
		await act(async () => controlsOf(renderer!.root).onSelectHarness("hermes"));
		expect(controlsOf(renderer!.root).harness).toBe("hermes");

		await act(async () => controlsOf(renderer!.root).onSelectProject("project-1"));
		await flush();

		expect(controlsOf(renderer!.root).projectId).toBe("project-1");
		expect(controlsOf(renderer!.root).harness).toBe("hermes");
	});

	it("keeps a picked model across the toggle when the new scope still offers it", async () => {
		const controls = await openSheet();

		await act(async () => controlsOf(renderer!.root).onSelectHarness("hermes"));
		await act(async () => controlsOf(renderer!.root).onSelectModel("m1"));
		await act(async () => controlsOf(renderer!.root).onSelectProject(STANDALONE_PROJECT));
		await flush();

		const after = controlsOf(renderer!.root);
		expect(after.projectId).toBe(STANDALONE_PROJECT);
		expect(after.modelSelection).toBe("m1");
		expect(after.modelLabel).toBe("Model One");
	});

	it("degrades a model the new scope dropped explicitly instead of swapping it silently", async () => {
		const controls = await openSheet();

		await act(async () => controlsOf(renderer!.root).onSelectHarness("hermes"));
		await act(async () => controlsOf(renderer!.root).onSelectModel("proj-only"));
		expect(controlsOf(renderer!.root).modelSelection).toBe("proj-only");

		await act(async () => controlsOf(renderer!.root).onSelectProject(STANDALONE_PROJECT));
		await flush();

		// Back to Automatic — visibly, with a line naming the loss.
		expect(controlsOf(renderer!.root).modelSelection).toBe("__auto__");
		expect(textContents(renderer!.root).some((line) => line.includes("proj-only") && line.includes("Automatic"))).toBe(true);
	});
});
