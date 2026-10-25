import { act, renderHook } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cloneFailureMessage, subscribeCloneProgress, usePreparedClone } from "./usePreparedClone";

const api = vi.hoisted(() => ({ POST: vi.fn() }));
vi.mock("../lib/api-client", () => ({
	apiClient: api,
	apiErrorDetails: (error: unknown): Record<string, unknown> | undefined => {
		if (typeof error !== "object" || error === null) return undefined;
		const details = (error as { details?: unknown }).details;
		return typeof details === "object" && details !== null ? (details as Record<string, unknown>) : undefined;
	},
	apiErrorMessage: () => "Request failed",
	getApiBaseUrl: () => "http://127.0.0.1:9900/",
}));
vi.mock("../lib/host-clients", () => ({
	baseUrlForHost: () => null,
	clientForSessionHost: () => api,
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

beforeEach(() => {
	api.POST.mockReset();
	EventSourceStub.instances = [];
	vi.stubGlobal("EventSource", EventSourceStub);
});

afterEach(() => {
	vi.unstubAllGlobals();
});

it.each([404, 405])("allows cloning on an older daemon without destination preflight (%s)", async (status) => {
	api.POST.mockResolvedValueOnce({ error: { message: "Route unavailable" }, response: { status } });
	const { result } = renderHook(() => usePreparedClone("older-host"));
	const input = { remoteUrl: "https://github.com/acme/repo.git", destinationParent: "/projects" };
	const signal = new AbortController().signal;
	await expect(result.current.checkDestination(input, signal)).resolves.toEqual({
		remoteUrl: input.remoteUrl, path: "", exists: false, canUseExisting: false,
	});
	expect(api.POST).toHaveBeenCalledWith("/api/v1/projects/clone/check", { body: input, signal });
	const conflict = { code: "CLONE_DESTINATION_EXISTS", message: "Destination exists" };
	api.POST.mockResolvedValueOnce({ error: conflict, response: { status: 409 } });
	await expect(result.current.prepare(input.remoteUrl, input.destinationParent)).rejects.toBe(conflict);
	expect(result.current.current()).toBeNull();
});

it.each([400, 401, 403, 500, 503])("preserves destination check failures other than missing routes (%s)", async (status) => {
	const error = { message: "Could not inspect destination" };
	api.POST.mockResolvedValueOnce({ error, response: { status } });
	const { result } = renderHook(() => usePreparedClone("remote-host"));
	await expect(result.current.checkDestination({ remoteUrl: "file:///source", destinationParent: "/projects" }, new AbortController().signal)).rejects.toBe(error);
});

it.each([404, 405])("preserves a legacy daemon's checkout when leaving (%s)", async (status) => {
	const prepared = { path: "/projects/source", remoteUrl: "file:///source", preparationId: "legacy-owned" };
	api.POST.mockResolvedValueOnce({ response: { status } });
	const { result } = renderHook(() => usePreparedClone("legacy-host"));
	await result.current.checkDestination({ remoteUrl: prepared.remoteUrl, destinationParent: "/projects" }, new AbortController().signal);
	api.POST.mockResolvedValueOnce({ data: prepared });
	await result.current.prepare(prepared.remoteUrl, "/projects");
	await act(() => result.current.cleanup());
	expect(result.current.current()).toBeNull();
	expect(api.POST).toHaveBeenCalledTimes(2);
	expect(api.POST.mock.calls.some(([path]) => path === "/api/v1/projects/clone/cleanup")).toBe(false);
});

it("only bypasses an existing destination for the current flow's owned checkout", async () => {
	const prepared = { path: "/projects/source", remoteUrl: "file:///source", preparationId: "owned" };
	const destination = { remoteUrl: prepared.remoteUrl, path: prepared.path, exists: true, canUseExisting: true };
	api.POST.mockResolvedValue({ data: destination });
	const { result } = renderHook(() => usePreparedClone());
	const input = { remoteUrl: prepared.remoteUrl, destinationParent: "~/projects" };
	await expect(result.current.checkDestination(input, new AbortController().signal)).resolves.toEqual(destination);
	api.POST.mockResolvedValueOnce({ data: prepared });
	await result.current.prepare(input.remoteUrl, input.destinationParent);
	await expect(result.current.checkDestination(input, new AbortController().signal)).resolves.toEqual({ ...destination, exists: false, canUseExisting: false });
	act(() => result.current.complete());
	await expect(result.current.checkDestination(input, new AbortController().signal)).resolves.toEqual(destination);
});

it("releases a checkout on a daemon supporting safe cleanup", async () => {
	const prepared = { path: "/projects/source", remoteUrl: "file:///source", preparationId: "owned" };
	api.POST.mockResolvedValueOnce({ data: { ...prepared, exists: false, canUseExisting: false } });
	const { result } = renderHook(() => usePreparedClone("current-host"));
	await result.current.checkDestination({ remoteUrl: prepared.remoteUrl, destinationParent: "/projects" }, new AbortController().signal);
	api.POST.mockResolvedValueOnce({ data: prepared });
	await result.current.prepare(prepared.remoteUrl, "/projects");
	api.POST.mockResolvedValueOnce({});
	await act(() => result.current.cleanup());
	expect(api.POST).toHaveBeenLastCalledWith("/api/v1/projects/clone/cleanup", {
		body: { path: prepared.path, preparationId: prepared.preparationId, keepCheckout: true },
	});
	expect(result.current.current()).toBeNull();
});

it("only releases files-preserving ownership when a legacy host upgrades during the flow", async () => {
	const prepared = { path: "/projects/source", remoteUrl: "file:///source", preparationId: "legacy-owned" };
	const input = { remoteUrl: prepared.remoteUrl, destinationParent: "/projects" };
	api.POST.mockResolvedValueOnce({ response: { status: 404 } });
	const { result } = renderHook(() => usePreparedClone("upgrading-host"));
	await result.current.checkDestination(input, new AbortController().signal);
	api.POST.mockResolvedValueOnce({ data: prepared });
	await result.current.prepare(input.remoteUrl, input.destinationParent);
	api.POST.mockResolvedValueOnce({ data: { ...prepared, exists: true, canUseExisting: true } });
	await result.current.checkDestination(input, new AbortController().signal);
	api.POST.mockResolvedValueOnce({});
	await act(() => result.current.cleanup());
	expect(api.POST).toHaveBeenLastCalledWith("/api/v1/projects/clone/cleanup", {
		body: { path: prepared.path, preparationId: prepared.preparationId, keepCheckout: true },
	});
});

it("cancels the active clone request", async () => {
	api.POST.mockImplementation((_path: string, { signal }: { signal: AbortSignal }) => new Promise((_resolve, reject) => {
		signal.addEventListener("abort", () => reject(new DOMException("Cancelled", "AbortError")), { once: true });
	}));
	const { result } = renderHook(() => usePreparedClone());
	const pending = result.current.prepare("file:///source", "~/projects");
	const rejection = expect(pending).rejects.toMatchObject({ name: "AbortError" });
	act(() => result.current.cancel());
	await rejection;
	expect(result.current.current()).toBeNull();
});

it("retains a checkout and its original input spelling for validation retries", async () => {
	const prepared = { path: "/Users/example/projects/source", remoteUrl: "file:///source", preparationId: "prepared" };
	api.POST.mockResolvedValue({ data: prepared });
	const { result } = renderHook(() => usePreparedClone());
	await result.current.prepare("file:///source", "~/projects");
	expect(result.current.current()).toEqual(prepared);
	expect(result.current.matches({ remoteUrl: "file:///source", destinationParent: "~/projects" })).toBe(true);
	expect(result.current.matches({ remoteUrl: "file:///other", destinationParent: "~/projects" })).toBe(false);
});

it("aborts a clone when its owning host changes", async () => {
	let requestSignal: AbortSignal | undefined;
	api.POST.mockImplementation((_path: string, { signal }: { signal: AbortSignal }) => {
		requestSignal = signal;
		return new Promise((_resolve, reject) => signal.addEventListener("abort", () => reject(new DOMException("Cancelled", "AbortError"))));
	});
	const { result, rerender } = renderHook(({ hostId }) => usePreparedClone(hostId), { initialProps: { hostId: "a" } });
	const pending = result.current.prepare("file:///source", "/projects");
	const rejection = expect(pending).rejects.toMatchObject({ name: "AbortError" });
	rerender({ hostId: "b" });
	await rejection;
	expect(requestSignal?.aborted).toBe(true);
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
	const preparedResult = {
		data: { path: "/tmp/dest/repo", remoteUrl: "https://example.com/owner/repo.git", preparationId: "prep-1" },
		error: undefined,
	};

	it("streams matching clone_progress frames and closes the stream when the clone settles", async () => {
		let resolvePost!: (value: typeof preparedResult) => void;
		api.POST.mockReturnValue(new Promise((resolve) => { resolvePost = resolve; }));
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
		api.POST.mockResolvedValue({ data: undefined, error: { code: "GIT_CLONE_FAILED", message: "Could not clone." } });
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
