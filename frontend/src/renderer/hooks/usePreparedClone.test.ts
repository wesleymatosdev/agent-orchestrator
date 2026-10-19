import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cloneFailureMessage, subscribeCloneProgress, usePreparedClone } from "./usePreparedClone";

const { apiClientPost } = vi.hoisted(() => ({ apiClientPost: vi.fn() }));

vi.mock("../lib/api-client", () => ({
	apiClient: { POST: apiClientPost },
	apiErrorDetails: (error: unknown): Record<string, unknown> | undefined => {
		if (typeof error !== "object" || error === null) return undefined;
		const details = (error as { details?: unknown }).details;
		return typeof details === "object" && details !== null ? (details as Record<string, unknown>) : undefined;
	},
	apiErrorMessage: (error: unknown, fallback = "Request failed"): string => {
		if (typeof error === "object" && error !== null) {
			const message = (error as { message?: unknown }).message;
			if (typeof message === "string" && message !== "") return message;
		}
		return fallback;
	},
	getApiBaseUrl: () => "http://127.0.0.1:9900/",
	hasTrustedApiBaseUrl: () => true,
}));

vi.mock("../lib/host-clients", () => ({
	baseUrlForHost: () => null,
	clientForSessionHost: () => ({ POST: apiClientPost }),
}));

class EventSourceStub extends EventTarget {
	static instances: EventSourceStub[] = [];
	url: string;
	closed = false;
	constructor(url: string) {
		super();
		this.url = url;
		EventSourceStub.instances.push(this);
	}
	close() {
		this.closed = true;
	}
	dispatch(name: string, data: string) {
		this.dispatchEvent(new MessageEvent(name, { data }));
	}
}

const preparedResult = {
	data: { path: "/tmp/dest/repo", remoteUrl: "https://example.com/owner/repo.git", preparationId: "prep-1" },
	error: undefined,
};

beforeEach(() => {
	EventSourceStub.instances = [];
	vi.stubGlobal("EventSource", EventSourceStub);
	apiClientPost.mockReset();
});

afterEach(() => {
	vi.unstubAllGlobals();
});

describe("cloneFailureMessage", () => {
	it("appends the git stderr tail from the error envelope", () => {
		const message = cloneFailureMessage(
			{ code: "GIT_CLONE_CANCELLED", message: "Repository cloning was cancelled.", details: { gitStderr: "fatal: the remote end hung up unexpectedly" } },
			"fallback",
		);
		expect(message).toBe("fallback\n\nfatal: the remote end hung up unexpectedly");
	});

	it("returns the base message when there is no stderr", () => {
		expect(cloneFailureMessage({ code: "GIT_CLONE_FAILED", message: "Could not clone." }, "fallback")).toBe("fallback");
		expect(cloneFailureMessage({ code: "X", message: "m", details: { gitStderr: "   " } }, "fallback")).toBe("fallback");
	});

	it("ignores a stderr tail that still carries URL credentials", () => {
		const message = cloneFailureMessage(
			{ code: "GIT_CLONE_FAILED", message: "x", details: { gitStderr: "fatal: Authentication failed for https://user:secret@github.com/acme/private.git" } },
			"fallback",
		);
		expect(message).toBe("fallback");
	});
});

describe("usePreparedClone progress", () => {
	it("streams matching clone_progress frames and closes the stream when the clone settles", async () => {
		let resolvePost!: (value: typeof preparedResult) => void;
		apiClientPost.mockReturnValue(new Promise((resolve) => { resolvePost = resolve; }));
		const onProgress = vi.fn();
		const { result } = renderHook(() => usePreparedClone());
		let prepared!: Promise<unknown>;
		act(() => {
			prepared = result.current.prepare("https://example.com/owner/repo.git", "/tmp/dest", onProgress);
		});

		const source = EventSourceStub.instances.at(-1)!;
		expect(source.url).toBe("http://127.0.0.1:9900/api/v1/projects/clone/progress");

		act(() => {
			source.dispatch("clone_progress", JSON.stringify({ remoteUrl: "https://example.com/owner/repo.git", stage: "receiving", percent: 43, message: "Receiving objects:  43%" }));
		});
		expect(onProgress).toHaveBeenCalledWith({ stage: "receiving", percent: 43, message: "Receiving objects:  43%" });

		// A clone this dialog did not start must not move this dialog's UI.
		act(() => {
			source.dispatch("clone_progress", JSON.stringify({ remoteUrl: "https://example.com/other.git", stage: "receiving", percent: 99, message: "other" }));
		});
		expect(onProgress).toHaveBeenCalledTimes(1);

		await act(async () => {
			resolvePost(preparedResult);
			await prepared;
		});
		expect(source.closed).toBe(true);
	});

	it("closes the stream when the clone fails", async () => {
		apiClientPost.mockResolvedValue({ data: undefined, error: { code: "GIT_CLONE_FAILED", message: "Could not clone." } });
		const { result } = renderHook(() => usePreparedClone());
		await expect(result.current.prepare("https://example.com/owner/repo.git", "/tmp/dest", () => {})).rejects.toBeTruthy();
		expect(EventSourceStub.instances.at(-1)!.closed).toBe(true);
	});

	it("skips the stream entirely when EventSource is unavailable", async () => {
		vi.stubGlobal("EventSource", undefined);
		const stop = subscribeCloneProgress(undefined, "https://example.com/x.git", () => {});
		expect(typeof stop).toBe("function");
		expect(() => stop()).not.toThrow();
		expect(EventSourceStub.instances).toHaveLength(0);
	});
});
