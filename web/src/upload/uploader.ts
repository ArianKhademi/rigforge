// The resumable multipart upload engine.
//
// It is deliberately free of browser APIs: the file, the HTTP transport and
// the persistence are passed in. The web app wires it to a File, XHR and
// localStorage; the CLI (scripts/upload-cli.ts) wires the very same class to a
// file on disk, fetch and a JSON file. The 2 GB resume test therefore
// exercises the code that ships in the browser.
//
// Protocol (see api/internal/upload/handler.go):
//   1. POST /api/uploads                  -> uploadId, partSize, partCount
//   2. POST /api/uploads/{id}/parts       -> presigned PUT URLs, in batches
//   3. PUT  <presigned url>               -> bytes go straight to the bucket
//   4. PUT  /api/uploads/{id}/parts/{n}   -> report the part's ETag
//   5. POST /api/uploads/{id}/complete    -> asset + processing job
// Resuming replaces step 1 with POST /api/uploads/{id}/reconcile, which
// returns the parts the server already has; only the others are sent.

import { ApiError } from "../api/client";
import type { CompletedUpload, CreatedUpload, PresignedPart, UploadInfo } from "../api/types";
import { initialState, reduce, remainingParts, type UploadEvent, type UploadState } from "./state";

/** The api calls the engine needs (a subset of api/client.ts). */
export interface UploadApi {
  createUpload(file: { filename: string; size: number; contentType: string }): Promise<CreatedUpload>;
  getUpload(id: string): Promise<UploadInfo>;
  reconcileUpload(id: string): Promise<UploadInfo>;
  presignParts(id: string, partNumbers: number[]): Promise<PresignedPart[]>;
  recordPart(id: string, partNumber: number, etag: string): Promise<void>;
  completeUpload(id: string, body: { name?: string; characterId?: string }): Promise<CompletedUpload>;
  abortUpload(id: string): Promise<void>;
}

/** Anything that can hand out byte ranges: a browser File, or a file on disk. */
export interface FileSource {
  name: string;
  size: number;
  type: string;
  lastModified: number;
  slice(start: number, end: number): Blob;
}

/** Sends one part to a presigned URL and resolves with the ETag header. */
export interface PartTransport {
  put(url: string, body: Blob, onProgress: (loaded: number) => void, signal: AbortSignal): Promise<string>;
}

/** What is remembered between sessions so an upload can be picked up again. */
export interface PersistedUpload {
  uploadId: string;
  fileName: string;
  fileSize: number;
  partCount: number;
  completedParts: number[];
  updatedAt: number;
}

export interface ResumeStore {
  load(key: string): PersistedUpload | null;
  save(key: string, value: PersistedUpload): void;
  remove(key: string): void;
}

export interface UploadOptions {
  /** Parts in flight at once. */
  concurrency?: number;
  /** Attempts per part before the upload stops as "interrupted". */
  maxAttempts?: number;
  /** Delay before retry number `attempt` (1 = first retry), in ms. */
  backoffMs?: (attempt: number) => number;
  /** How many part URLs to request per presign call. */
  presignBatch?: number;
  /** Presigned URLs older than this are thrown away and requested again. */
  urlMaxAgeMs?: number;
  /** Passed to complete: asset name and the character to apply the motion to. */
  name?: string;
  characterId?: string;
  /** If false, stop once every part is uploaded and leave completing to the caller. */
  complete?: boolean;
  sleep?: (ms: number, signal: AbortSignal) => Promise<void>;
  now?: () => number;
}

export interface UploadResult {
  uploadId: string;
  /** Null when the upload ran with complete: false. */
  completed: CompletedUpload | null;
  /** Part numbers this session actually sent, in completion order. */
  sentParts: number[];
  /** Part numbers that were already on the server and were not sent again. */
  skippedParts: number[];
}

/** Thrown from start()/resume() when the upload stops short of finishing. */
export class UploadStopped extends Error {
  constructor(readonly reason: "paused" | "interrupted" | "cancelled") {
    super(`upload ${reason}`);
    this.name = "UploadStopped";
  }
}

/** Identifies "the same file" across sessions without reading its bytes. */
export function fingerprint(source: FileSource): string {
  return `${source.name}:${source.size}:${source.lastModified}`;
}

// Exponential backoff with jitter: 0.5 s, 1 s, 2 s, ... capped at 15 s. The
// random factor keeps four parts that failed together (a dropped connection)
// from all retrying at the same instant.
export function defaultBackoff(attempt: number): number {
  const base = Math.min(15_000, 500 * 2 ** (attempt - 1));
  return base * (0.75 + Math.random() * 0.5);
}

function abortableSleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) return reject(signal.reason);
    const timer = setTimeout(resolve, ms);
    signal.addEventListener(
      "abort",
      () => {
        clearTimeout(timer);
        reject(signal.reason);
      },
      { once: true },
    );
  });
}

export class ResumableUpload {
  private current: UploadState;
  private listeners = new Set<(state: UploadState) => void>();
  private abort = new AbortController();
  private stopReason: "paused" | "cancelled" | null = null;
  private urls = new Map<number, { url: string; fetchedAt: number }>();
  private presigning: Promise<unknown> = Promise.resolve();
  private sentParts: number[] = [];
  private skippedParts: number[] = [];
  private readonly key: string;
  private readonly opts: Required<Omit<UploadOptions, "name" | "characterId">> & Pick<UploadOptions, "name" | "characterId">;

  constructor(
    private readonly source: FileSource,
    private readonly deps: { api: UploadApi; transport: PartTransport; store: ResumeStore },
    options: UploadOptions = {},
  ) {
    this.current = initialState(source.name, source.size);
    this.key = fingerprint(source);
    this.opts = {
      concurrency: 4,
      maxAttempts: 5,
      backoffMs: defaultBackoff,
      presignBatch: 8,
      // The api signs URLs for 15 minutes; refresh well before that.
      urlMaxAgeMs: 10 * 60_000,
      complete: true,
      sleep: abortableSleep,
      now: Date.now,
      ...options,
    };
  }

  get state(): UploadState {
    return this.current;
  }

  subscribe(listener: (state: UploadState) => void): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  private dispatch(event: UploadEvent): void {
    this.current = reduce(this.current, event);
    for (const listener of this.listeners) listener(this.current);
  }

  /** Change what complete will send (the user may pick a character mid-upload). */
  configure(change: Pick<UploadOptions, "name" | "characterId">): void {
    Object.assign(this.opts, change);
  }

  /**
   * Run the upload to the end. Rejects with UploadStopped if it is paused,
   * cancelled or interrupted; call resume() to continue a paused or
   * interrupted upload.
   */
  async start(): Promise<UploadResult> {
    this.abort = new AbortController();
    this.stopReason = null;
    this.sentParts = [];
    this.skippedParts = [];
    this.urls.clear();
    try {
      await this.prepare();
      await this.sendRemainingParts();
      return await this.finish();
    } catch (err) {
      const failure = this.classify(err);
      if (failure instanceof UploadStopped && failure.reason === "interrupted") {
        // The other in-flight parts were aborted along with the failed one;
        // "paused" resets them to pending (the phase stays interrupted).
        this.dispatch({ type: "paused" });
      }
      throw failure;
    }
  }

  /** Continue after a pause or an interruption. Server state decides what is left. */
  resume(): Promise<UploadResult> {
    return this.start();
  }

  /** Stop sending. Completed parts stay on the server; resume() carries on. */
  pause(): void {
    this.stopReason = "paused";
    this.abort.abort(new UploadStopped("paused"));
    this.dispatch({ type: "paused" });
  }

  /** Abort the upload on the server and forget it. */
  async cancel(): Promise<void> {
    this.stopReason = "cancelled";
    this.abort.abort(new UploadStopped("cancelled"));
    const { uploadId } = this.current;
    this.deps.store.remove(this.key);
    this.dispatch({ type: "cancelled" });
    if (uploadId) {
      // Best effort: if this fails the api's reaper aborts the upload after 24 h.
      await this.deps.api.abortUpload(uploadId).catch(() => undefined);
    }
  }

  // ---- step 1: create, or find out what the server already has ----

  private async prepare(): Promise<void> {
    this.dispatch({ type: "preparing" });
    const { api, store } = this.deps;

    const saved = store.load(this.key);
    if (saved && saved.fileSize === this.source.size) {
      const existing = await this.lookup(saved.uploadId);
      if (existing?.status === "uploading" && existing.size === this.source.size) {
        // Resume. The list of finished parts comes from the server, not from
        // local storage: the server (which asked the bucket) is the authority.
        const completed = existing.parts.map((p) => p.partNumber);
        this.skippedParts = completed;
        this.persist(existing.uploadId, existing.partCount, completed);
        this.dispatch({
          type: "prepared",
          uploadId: existing.uploadId,
          partSize: existing.partSize,
          partCount: existing.partCount,
          completed,
        });
        return;
      }
      if (existing?.status === "completed") {
        // Every part was sent and complete succeeded, but this client never
        // saw the response. Nothing to upload; finish() asks the api again.
        const all = Array.from({ length: existing.partCount }, (_, i) => i + 1);
        this.skippedParts = all;
        this.dispatch({
          type: "prepared",
          uploadId: existing.uploadId,
          partSize: existing.partSize,
          partCount: existing.partCount,
          completed: all,
        });
        return;
      }
      store.remove(this.key); // aborted, reaped or unknown: start over
    }

    const created = await api.createUpload({
      filename: this.source.name,
      size: this.source.size,
      contentType: this.source.type || "video/mp4",
    });
    this.persist(created.uploadId, created.partCount, []);
    this.dispatch({
      type: "prepared",
      uploadId: created.uploadId,
      partSize: created.partSize,
      partCount: created.partCount,
      completed: [],
    });
  }

  /** The server's view of a saved upload, or null if it no longer exists. */
  private async lookup(uploadId: string): Promise<UploadInfo | null> {
    const { api } = this.deps;
    try {
      // Reconcile first: it makes the api adopt any part that reached the
      // bucket but was never reported (the previous session died in between).
      return await api.reconcileUpload(uploadId);
    } catch (err) {
      if (err instanceof ApiError && err.status === 404) return null;
      // 409: the upload is no longer in flight. A plain GET tells us whether
      // it was completed or aborted.
      if (err instanceof ApiError && err.status === 409) return api.getUpload(uploadId);
      throw err;
    }
  }

  // ---- steps 2-4: send the parts, a few at a time ----

  private async sendRemainingParts(): Promise<void> {
    // This run's own controller. A worker from an earlier, paused run can
    // still be unwinding when resume() starts the next one; holding on to the
    // controller (instead of reading this.abort later) guarantees such a
    // straggler can only ever abort its own run.
    const run = this.abort;
    const queue = remainingParts(this.current);
    // A pool of workers pulling from one queue: at most `concurrency` parts
    // are in flight, and a slow part never blocks the others.
    const worker = async (): Promise<void> => {
      for (let part = queue.shift(); part !== undefined; part = queue.shift()) {
        await this.sendPart(part, queue, run.signal);
      }
    };
    const workers = Array.from({ length: Math.min(this.opts.concurrency, queue.length) }, worker);
    // If one worker gives up, stop the rest promptly instead of letting them
    // keep sending while the upload is already marked interrupted.
    await Promise.all(
      workers.map((w) =>
        w.catch((err) => {
          run.abort(err);
          throw err;
        }),
      ),
    );
  }

  private async sendPart(part: number, upcoming: number[], signal: AbortSignal): Promise<void> {
    const { api, transport } = this.deps;
    const uploadId = this.current.uploadId!;
    const { size } = this.current.parts[part - 1]!;
    const offset = (part - 1) * this.current.partSize;

    for (let attempt = 1; ; attempt++) {
      signal.throwIfAborted();
      this.dispatch({ type: "part_started", part });
      try {
        const url = await this.urlFor(part, upcoming);
        // The upload may have been stopped while the URL was being fetched;
        // sending the part now would only put bytes in the bucket that this
        // run then ignores.
        signal.throwIfAborted();
        // slice() does not read anything yet: the bytes are pulled from disk
        // as the request body is sent, one part at a time.
        const etag = await transport.put(
          url,
          this.source.slice(offset, offset + size),
          (loaded) => {
            if (!signal.aborted) this.dispatch({ type: "part_progress", part, loaded });
          },
          signal,
        );
        // Paused or cancelled while the last bytes were in flight: leave the
        // state alone. The part is in the bucket, and the next resume adopts
        // it through reconcile instead of sending it again.
        signal.throwIfAborted();
        await api.recordPart(uploadId, part, etag);
        this.sentParts.push(part);
        this.persist(uploadId, this.current.parts.length, [...this.completedParts(), part]);
        this.dispatch({ type: "part_done", part });
        return;
      } catch (err) {
        if (signal.aborted) throw signal.reason;
        const message = err instanceof Error ? err.message : String(err);
        // The upload itself is gone (aborted elsewhere): retrying is pointless.
        if (err instanceof ApiError && (err.status === 404 || err.status === 409)) {
          this.deps.store.remove(this.key);
          this.dispatch({ type: "error", error: message });
          throw err;
        }
        this.urls.delete(part); // a rejected URL may simply have expired
        if (attempt >= this.opts.maxAttempts) {
          this.dispatch({ type: "part_failed", part, error: message });
          throw new UploadStopped("interrupted");
        }
        this.dispatch({ type: "part_retry", part, error: message });
        await this.opts.sleep(this.opts.backoffMs(attempt), signal);
      }
    }
  }

  /** A presigned URL for `part`, requesting a batch that covers upcoming parts too. */
  private urlFor(part: number, upcoming: number[]): Promise<string> {
    const fresh = (n: number): boolean => {
      const cached = this.urls.get(n);
      return cached !== undefined && this.opts.now() - cached.fetchedAt < this.opts.urlMaxAgeMs;
    };
    // Requests are chained one after another. The workers all ask for their
    // first URL at the same moment; without the chain each would request its
    // own overlapping batch, with it the first batch answers the others.
    const result = this.presigning.then(async () => {
      if (!fresh(part)) {
        const batch = [part, ...upcoming.filter((n) => !fresh(n))].slice(0, this.opts.presignBatch);
        const presigned = await this.deps.api.presignParts(this.current.uploadId!, batch);
        const fetchedAt = this.opts.now();
        for (const p of presigned) this.urls.set(p.partNumber, { url: p.url, fetchedAt });
      }
      return this.urls.get(part)!.url;
    });
    this.presigning = result.catch(() => undefined); // a failed request must not block the next
    return result;
  }

  // ---- step 5: complete ----

  private async finish(): Promise<UploadResult> {
    const uploadId = this.current.uploadId!;
    const result: UploadResult = {
      uploadId,
      completed: null,
      sentParts: [...this.sentParts],
      skippedParts: [...this.skippedParts].sort((a, b) => a - b),
    };
    if (!this.opts.complete) return result;

    this.dispatch({ type: "completing" });
    try {
      // Complete is idempotent on the server, so a retry after a lost
      // response returns the same asset and job.
      result.completed = await this.deps.api.completeUpload(uploadId, {
        name: this.opts.name,
        characterId: this.opts.characterId,
      });
    } catch (err) {
      this.dispatch({ type: "error", error: err instanceof Error ? err.message : String(err) });
      throw err;
    }
    this.deps.store.remove(this.key);
    this.dispatch({ type: "completed", ...result.completed });
    return result;
  }

  // ---- helpers ----

  private completedParts(): number[] {
    return this.current.parts.filter((p) => p.status === "done").map((p) => p.number);
  }

  private persist(uploadId: string, partCount: number, completedParts: number[]): void {
    this.deps.store.save(this.key, {
      uploadId,
      fileName: this.source.name,
      fileSize: this.source.size,
      partCount,
      completedParts: [...new Set(completedParts)].sort((a, b) => a - b),
      updatedAt: this.opts.now(),
    });
  }

  /** Turn whatever was thrown into the error start() should reject with. */
  private classify(err: unknown): unknown {
    if (this.stopReason) return new UploadStopped(this.stopReason);
    if (err instanceof UploadStopped) return err;
    if (this.current.phase !== "error" && this.current.phase !== "interrupted") {
      this.dispatch({ type: "error", error: err instanceof Error ? err.message : String(err) });
    }
    return err;
  }
}
