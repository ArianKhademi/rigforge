// The upload state machine as a pure reducer: (state, event) -> state.
//
// The engine in uploader.ts does the I/O and feeds events in here; the UI
// renders whatever state comes out. Keeping the transitions pure means every
// rule ("a paused upload forgets in-flight progress", "a part that was done
// stays done") is unit-tested without a network or a browser.

export type PartStatus = "pending" | "uploading" | "done" | "failed";

export interface PartState {
  number: number; // 1-based, as in S3
  size: number; // bytes in this part
  status: PartStatus;
  loaded: number; // bytes sent so far in the current attempt
  attempts: number; // attempts made in this session
  /** True if the part was already on the server when this session started. */
  resumed: boolean;
}

export type Phase =
  | "idle"
  | "preparing" // creating the upload or asking the server what it already has
  | "uploading"
  | "paused" // by the user
  | "interrupted" // a part ran out of retries; nothing is lost, resume() continues
  | "completing"
  | "done"
  | "cancelled"
  | "error"; // not resumable (the server refused the upload)

export interface UploadState {
  phase: Phase;
  fileName: string;
  fileSize: number;
  uploadId: string | null;
  partSize: number;
  parts: PartState[];
  error: string | null;
  assetId: string | null;
  jobId: string | null;
}

export type UploadEvent =
  | { type: "preparing" }
  // The server's answer: the part plan and which parts it already has.
  | { type: "prepared"; uploadId: string; partSize: number; partCount: number; completed: number[] }
  | { type: "part_started"; part: number }
  | { type: "part_progress"; part: number; loaded: number }
  | { type: "part_done"; part: number }
  | { type: "part_retry"; part: number; error: string }
  | { type: "part_failed"; part: number; error: string }
  | { type: "paused" }
  | { type: "completing" }
  | { type: "completed"; assetId: string; jobId: string }
  | { type: "cancelled" }
  | { type: "error"; error: string };

export function initialState(fileName: string, fileSize: number): UploadState {
  return {
    phase: "idle",
    fileName,
    fileSize,
    uploadId: null,
    partSize: 0,
    parts: [],
    error: null,
    assetId: null,
    jobId: null,
  };
}

/** Size of part n: every part is partSize bytes except the last. */
export function partLength(fileSize: number, partSize: number, partCount: number, n: number): number {
  return n === partCount ? fileSize - (partCount - 1) * partSize : partSize;
}

function updatePart(state: UploadState, n: number, change: (p: PartState) => PartState): UploadState {
  return { ...state, parts: state.parts.map((p) => (p.number === n ? change(p) : p)) };
}

export function reduce(state: UploadState, event: UploadEvent): UploadState {
  switch (event.type) {
    case "preparing":
      return { ...state, phase: "preparing", error: null };

    case "prepared": {
      const have = new Set(event.completed);
      const parts: PartState[] = [];
      for (let n = 1; n <= event.partCount; n++) {
        const done = have.has(n);
        const size = partLength(state.fileSize, event.partSize, event.partCount, n);
        parts.push({ number: n, size, status: done ? "done" : "pending", loaded: done ? size : 0, attempts: 0, resumed: done });
      }
      return { ...state, phase: "uploading", uploadId: event.uploadId, partSize: event.partSize, parts, error: null };
    }

    case "part_started":
      return updatePart(state, event.part, (p) => ({ ...p, status: "uploading", loaded: 0, attempts: p.attempts + 1 }));

    case "part_progress":
      // Progress events can arrive after the part finished or was reset; only
      // a part that is actually uploading accepts them.
      return updatePart(state, event.part, (p) =>
        p.status === "uploading" ? { ...p, loaded: Math.min(p.size, event.loaded) } : p,
      );

    case "part_done":
      return updatePart(state, event.part, (p) => ({ ...p, status: "done", loaded: p.size }));

    case "part_retry":
      // The attempt failed and will be retried: its bytes do not count.
      return updatePart(state, event.part, (p) => ({ ...p, status: "pending", loaded: 0 }));

    case "part_failed":
      return {
        ...updatePart(state, event.part, (p) => ({ ...p, status: "failed", loaded: 0 })),
        phase: "interrupted",
        error: event.error,
      };

    case "paused":
      if (state.phase !== "uploading" && state.phase !== "interrupted") return state;
      // In-flight PUTs are aborted, and S3 discards a partial part, so their
      // progress is forgotten. Completed parts are untouched.
      return {
        ...state,
        phase: state.phase === "interrupted" ? "interrupted" : "paused",
        parts: state.parts.map((p) => (p.status === "uploading" ? { ...p, status: "pending", loaded: 0 } : p)),
      };

    case "completing":
      return { ...state, phase: "completing" };

    case "completed":
      return { ...state, phase: "done", assetId: event.assetId, jobId: event.jobId, error: null };

    case "cancelled":
      return { ...state, phase: "cancelled" };

    case "error":
      return { ...state, phase: "error", error: event.error };
  }
}

// ---- selectors ----

export function uploadedBytes(state: UploadState): number {
  return state.parts.reduce((sum, p) => sum + p.loaded, 0);
}

/** Overall progress, 0..1. */
export function progress(state: UploadState): number {
  if (state.phase === "done") return 1;
  return state.fileSize > 0 ? uploadedBytes(state) / state.fileSize : 0;
}

export function countParts(state: UploadState, status: PartStatus): number {
  return state.parts.filter((p) => p.status === status).length;
}

/** Parts still to send, in order. Failed parts are retried on resume. */
export function remainingParts(state: UploadState): number[] {
  return state.parts.filter((p) => p.status === "pending" || p.status === "failed").map((p) => p.number);
}
