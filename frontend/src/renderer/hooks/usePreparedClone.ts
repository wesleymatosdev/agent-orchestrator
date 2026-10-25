import { useCallback, useEffect, useRef, useState } from "react";
import type { components } from "../../api/schema";
import { apiClient, apiErrorDetails, apiErrorMessage, getApiBaseUrl } from "../lib/api-client";
import { baseUrlForHost, clientForSessionHost } from "../lib/host-clients";

type PreparedClone = components["schemas"]["ClonePreparationResult"];

/** One live clone progress update relayed from the daemon's SSE stream. */
export type CloneProgress = {
	stage: string;
	percent: number | null;
	message: string;
};

/**
 * Human message for a failed clone: the caller's safe guidance text plus, when
 * present, git's own stderr tail — the only real diagnosis for a failed or
 * cancelled clone. The tail arrives credential-redacted in
 * error.details.gitStderr.
 */
export function cloneFailureMessage(error: unknown, fallback: string): string {
	// The base stays the caller's safe guidance text: envelope messages (and
	// git output generally) can embed credentials from the remote URL. The
	// daemon redacts the stderr tail before it ships; still ignore it when
	// it somehow carries an obvious secret form.
	const details = apiErrorDetails(error);
	const stderr = typeof details?.gitStderr === "string" ? details.gitStderr.trim() : "";
	const safe = /:\/\/[^\s:@/]+:[^\s@/]+@/.test(stderr) ? "" : stderr;
	return safe ? `${fallback}\n\n${safe}` : fallback;
}

/**
 * Subscribes to the daemon's clone progress stream for the duration of one
 * clone/prepare call. Events are broadcast to every subscriber; remoteUrl
 * filters out clones this dialog did not start. Returns a stop function; the
 * stream is not replayed, so subscribers that attach late simply miss frames.
 */
export function subscribeCloneProgress(
	hostId: string | undefined,
	remoteUrl: string,
	onProgress: (progress: CloneProgress) => void,
): () => void {
	if (typeof EventSource === "undefined") return () => {};
	const baseUrl = hostId ? baseUrlForHost(hostId) : getApiBaseUrl();
	if (!baseUrl) return () => {};
	const source = new EventSource(`${baseUrl.replace(/\/+$/, "")}/api/v1/projects/clone/progress`);
	source.addEventListener("clone_progress", (event) => {
		let payload: { remoteUrl?: string; stage?: string; percent?: number; message?: string } = {};
		try {
			payload = JSON.parse((event as MessageEvent<string>).data || "{}") as typeof payload;
		} catch {
			return;
		}
		// The daemon echoes the request's remoteUrl verbatim; ignore other
		// in-flight clones so two dialogs never cross wires.
		if (payload.remoteUrl && payload.remoteUrl !== remoteUrl) return;
		onProgress({
			stage: payload.stage ?? "",
			percent: typeof payload.percent === "number" ? payload.percent : null,
			message: payload.message ?? "",
		});
	});
	return () => source.close();
}

export function usePreparedClone(hostId?: string) {
	const currentRef = useRef<PreparedClone | null>(null);
	const inputRef = useRef<{ remoteUrl: string; destinationParent: string } | null>(null);
	const [isCleaning, setIsCleaning] = useState(false);
	const activeRequest = useRef<AbortController | null>(null);
	const supportsCheckoutRelease = useRef(!hostId);

	useEffect(() => {
		supportsCheckoutRelease.current = !hostId;
		return () => {
			activeRequest.current?.abort();
			currentRef.current = null;
			inputRef.current = null;
		};
	}, [hostId]);

	const checkDestination = useCallback(async (input: { remoteUrl: string; destinationParent: string }, signal: AbortSignal) => {
		const { data, error, response } = await clientForSessionHost(hostId).POST("/api/v1/projects/clone/check", { body: input, signal });
		if (signal.aborted) throw new DOMException("Cancelled", "AbortError");
		if (response?.status === 404 || response?.status === 405) {
			supportsCheckoutRelease.current = false;
			// Older daemons lack this optional preflight. Keep the dialog's path
			// preview and let prepare enforce destination conflicts as before.
			return { remoteUrl: input.remoteUrl, path: "", exists: false, canUseExisting: false };
		}
		if (error) throw error;
		if (!data) throw new Error("Could not check the destination. Retry or choose another folder.");
		supportsCheckoutRelease.current = true;
		const owned = currentRef.current;
		if (owned && owned.path === data.path && owned.remoteUrl === data.remoteUrl && inputRef.current?.destinationParent === input.destinationParent) {
			return { ...data, exists: false, canUseExisting: false };
		}
		return data;
	}, [hostId]);

	const prepare = useCallback(async (remoteUrl: string, destinationParent: string, onProgress?: (progress: CloneProgress) => void) => {
		const controller = new AbortController();
		activeRequest.current = controller;
		// Open the progress stream before the clone starts so the first git
		// frames are not missed, and close it when the call settles.
		const stopProgress = onProgress ? subscribeCloneProgress(hostId, remoteUrl, onProgress) : null;
		try {
			const { data, error } = await (hostId ? clientForSessionHost(hostId) : apiClient).POST("/api/v1/projects/clone/prepare", {
				body: { remoteUrl, destinationParent },
				signal: controller.signal,
			});
			if (error) throw error;
			if (!data) throw new Error("Could not clone repository");
			if (controller.signal.aborted) throw new DOMException("Cancelled", "AbortError");
			currentRef.current = data;
			inputRef.current = { remoteUrl, destinationParent };
			return data;
		} finally {
			stopProgress?.();
			if (activeRequest.current === controller) activeRequest.current = null;
		}
	}, [hostId]);

	const matches = useCallback((input: { remoteUrl: string; destinationParent: string }) =>
		inputRef.current?.remoteUrl === input.remoteUrl && inputRef.current.destinationParent === input.destinationParent, []);

	const cancel = useCallback(() => activeRequest.current?.abort(), []);

	const cleanup = useCallback(async () => {
		const current = currentRef.current;
		if (!current) return;
		if (!supportsCheckoutRelease.current) {
			// Legacy daemons cannot release ownership without deleting files.
			// Preserve the checkout rather than send an unsupported keep flag.
			currentRef.current = null;
			inputRef.current = null;
			return;
		}
		setIsCleaning(true);
		try {
			const { error } = await (hostId ? clientForSessionHost(hostId) : apiClient).POST("/api/v1/projects/clone/cleanup", {
				body: { path: current.path, preparationId: current.preparationId, keepCheckout: true },
			});
			if (error) throw new Error(apiErrorMessage(error, "Could not keep the cloned checkout"));
			currentRef.current = null;
			inputRef.current = null;
		} finally {
			setIsCleaning(false);
		}
	}, [hostId]);

	const complete = useCallback(() => {
		currentRef.current = null;
		inputRef.current = null;
	}, []);

	const current = useCallback(() => currentRef.current, []);

	return { cancel, checkDestination, cleanup, complete, current, isCleaning, matches, prepare };
}
