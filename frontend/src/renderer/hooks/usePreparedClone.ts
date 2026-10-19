import { useCallback, useRef, useState } from "react";
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
 * Human message for a failed clone: the daemon's envelope message plus, when
 * present, git's own stderr tail — the only real diagnosis for a failed or
 * cancelled clone. The stderr tail arrives in error.details.gitStderr.
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
	const [isCleaning, setIsCleaning] = useState(false);

	const prepare = useCallback(async (remoteUrl: string, destinationParent: string, onProgress?: (progress: CloneProgress) => void) => {
		// Open the progress stream before the clone starts so the first git
		// frames are not missed, and close it when the call settles.
		const stopProgress = onProgress ? subscribeCloneProgress(hostId, remoteUrl, onProgress) : null;
		try {
			const { data, error } = await (hostId ? clientForSessionHost(hostId) : apiClient).POST("/api/v1/projects/clone/prepare", {
				body: { remoteUrl, destinationParent },
			});
			if (error) throw error;
			if (!data) throw new Error("Could not clone repository");
			currentRef.current = data;
			return data;
		} finally {
			stopProgress?.();
		}
	}, [hostId]);

	const cleanup = useCallback(async () => {
		const current = currentRef.current;
		if (!current) return;
		setIsCleaning(true);
		try {
			const { error } = await (hostId ? clientForSessionHost(hostId) : apiClient).POST("/api/v1/projects/clone/cleanup", {
				body: { path: current.path, preparationId: current.preparationId },
			});
			if (error) throw new Error(apiErrorMessage(error, "Could not clean up prepared clone"));
			currentRef.current = null;
		} finally {
			setIsCleaning(false);
		}
	}, [hostId]);

	const complete = useCallback(() => {
		currentRef.current = null;
	}, []);

	const current = useCallback(() => currentRef.current, []);

	return { cleanup, complete, current, isCleaning, prepare };
}
