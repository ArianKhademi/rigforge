import { describe, expect, it } from "vitest";
import { ApiError } from "../api/client";
import type { UploadInfo } from "../api/types";
import { memoryStore } from "./persistence";
import { progress } from "./state";
import { PartRejected } from "./transport";
import {
  fingerprint,
  ResumableUpload,
  UploadStopped,
  type FileSource,
  type PartTransport,
  type UploadApi,
  type UploadOptions,
} from "./uploader";

const PART_SIZE = 10;

/** A file whose content is predictable: byte i is (i % 251). */
function fakeFile(size: number, name = "clip.mp4"): FileSource {
  const bytes = Uint8Array.from({ length: size }, (_, i) => i % 251);
  return {
    name,
    size,
    type: "video/mp4",
    lastModified: 1_700_000_000_000,
    slice: (start, end) => new Blob([bytes.slice(start, end)]),
  };
}

interface ServerUpload {
  id: string;
  size: number;
  partCount: number;
  status: "uploading" | "completed" | "aborted";
  recorded: Map<number, string>; // what the api knows (reported ETags)
  bucket: Map<number, { etag: string; size: number }>; // what actually reached storage
}

/**
 * An in-memory api + bucket. It implements both sides the engine talks to
 * (UploadApi and PartTransport) and counts everything, so tests can assert
 * exactly which parts were sent and how often.
 */
class FakeServer implements UploadApi, PartTransport {
  uploads = new Map<string, ServerUpload>();
  puts: number[] = []; // part number of every PUT that reached the bucket
  putAttempts: number[] = []; // every PUT attempt, including failed ones
  presignCalls: number[][] = [];
  completeCalls: { name?: string; characterId?: string }[] = [];
  createCalls = 0;
  inFlight = 0;
  maxInFlight = 0;

  /** Scripted behaviour per part: called on each PUT attempt, may throw or wait. */
  onPut: (part: number, attempt: number) => Promise<void> | void = () => undefined;
  /** Fail the ETag report (the client "dies" after the PUT succeeded). */
  failRecord: (part: number) => boolean = () => false;
  private attempts = new Map<number, number>();
  private nextId = 1;

  async createUpload(file: { filename: string; size: number }) {
    this.createCalls++;
    const id = `upload-${this.nextId++}`;
    const partCount = Math.ceil(file.size / PART_SIZE);
    this.uploads.set(id, { id, size: file.size, partCount, status: "uploading", recorded: new Map(), bucket: new Map() });
    return { uploadId: id, assetId: `asset-${id}`, partSize: PART_SIZE, partCount };
  }

  private info(u: ServerUpload): UploadInfo {
    return {
      uploadId: u.id,
      assetId: `asset-${u.id}`,
      filename: "clip.mp4",
      size: u.size,
      contentType: "video/mp4",
      partSize: PART_SIZE,
      partCount: u.partCount,
      status: u.status,
      parts: [...u.recorded].sort(([a], [b]) => a - b).map(([partNumber, etag]) => ({ partNumber, etag, recordedAt: "" })),
    };
  }

  private find(id: string): ServerUpload {
    const u = this.uploads.get(id);
    if (!u) throw new ApiError(404, "not_found", "upload not found");
    return u;
  }

  async getUpload(id: string) {
    return this.info(this.find(id));
  }

  async reconcileUpload(id: string) {
    const u = this.find(id);
    if (u.status !== "uploading") throw new ApiError(409, "not_uploading", `upload is ${u.status}`);
    for (const [n, part] of u.bucket) u.recorded.set(n, part.etag); // adopt what the bucket has
    return this.info(u);
  }

  async presignParts(id: string, partNumbers: number[]) {
    const u = this.find(id);
    if (u.status !== "uploading") throw new ApiError(409, "not_uploading", `upload is ${u.status}`);
    this.presignCalls.push([...partNumbers]);
    return partNumbers.map((partNumber) => ({ partNumber, url: `https://bucket.test/${id}?partNumber=${partNumber}` }));
  }

  async put(url: string, body: Blob, onProgress: (loaded: number) => void, signal: AbortSignal): Promise<string> {
    const parsed = new URL(url);
    const id = parsed.pathname.slice(1);
    const part = Number(parsed.searchParams.get("partNumber"));
    const attempt = (this.attempts.get(part) ?? 0) + 1;
    this.attempts.set(part, attempt);
    this.putAttempts.push(part);
    this.inFlight++;
    this.maxInFlight = Math.max(this.maxInFlight, this.inFlight);
    try {
      onProgress(Math.floor(body.size / 2));
      // Let other parts start, so concurrency is observable, and honour abort.
      await Promise.race([
        Promise.resolve(this.onPut(part, attempt)).then(() => new Promise((r) => setTimeout(r, 1))),
        new Promise((_, reject) => signal.addEventListener("abort", () => reject(signal.reason), { once: true })),
      ]);
      signal.throwIfAborted();
      const etag = `etag-${part}-${body.size}`;
      this.find(id).bucket.set(part, { etag, size: body.size });
      this.puts.push(part);
      onProgress(body.size);
      return `"${etag}"`; // S3 quotes ETags
    } finally {
      this.inFlight--;
    }
  }

  async recordPart(id: string, partNumber: number, etag: string) {
    const u = this.find(id);
    if (u.status !== "uploading") throw new ApiError(409, "not_uploading", `upload is ${u.status}`);
    if (this.failRecord(partNumber)) throw new Error("network error while reporting the ETag");
    u.recorded.set(partNumber, etag.replaceAll('"', ""));
  }

  async completeUpload(id: string, body: { name?: string; characterId?: string }) {
    const u = this.find(id);
    this.completeCalls.push(body);
    if (u.status === "aborted") throw new ApiError(409, "aborted", "upload was aborted");
    if (u.status === "uploading") {
      const missing = Array.from({ length: u.partCount }, (_, i) => i + 1).filter((n) => !u.recorded.has(n));
      if (missing.length > 0) throw new ApiError(409, "missing_parts", "upload cannot complete", { missing });
      u.status = "completed";
    }
    return { assetId: `asset-${id}`, jobId: `job-${id}` };
  }

  async abortUpload(id: string) {
    this.find(id).status = "aborted";
  }

  /** How many times each part reached the bucket. */
  putCounts(): Record<number, number> {
    const counts: Record<number, number> = {};
    for (const part of this.puts) counts[part] = (counts[part] ?? 0) + 1;
    return counts;
  }
}

function setup(fileSize = 95, options: UploadOptions = {}) {
  const server = new FakeServer();
  const store = memoryStore();
  const file = fakeFile(fileSize);
  const delays: number[] = [];
  const make = (extra: UploadOptions = {}) =>
    new ResumableUpload(
      file,
      { api: server, transport: server, store },
      {
        // Record the backoff instead of waiting for it.
        sleep: async (ms) => void delays.push(ms),
        backoffMs: (attempt) => 100 * attempt,
        ...options,
        ...extra,
      },
    );
  return { server, store, file, delays, make, upload: make() };
}

const range = (from: number, to: number) => Array.from({ length: to - from + 1 }, (_, i) => from + i);

describe("a fresh upload", () => {
  it("sends every part once, records it and completes", async () => {
    const { server, store, upload } = setup(95, { name: "My dance", characterId: "default" });

    const result = await upload.start();

    // 95 bytes in 10-byte parts: nine full parts and a 5-byte tail.
    expect(result.sentParts.toSorted((a, b) => a - b)).toEqual(range(1, 10));
    expect(result.skippedParts).toEqual([]);
    expect(server.putCounts()).toEqual(Object.fromEntries(range(1, 10).map((n) => [n, 1])));
    const stored = server.uploads.get(result.uploadId)!;
    expect(stored.bucket.get(1)!.size).toBe(10);
    expect(stored.bucket.get(10)!.size).toBe(5);
    // ETags are reported without S3's quotes having been lost or doubled.
    expect(stored.recorded.get(10)).toBe("etag-10-5");

    expect(server.completeCalls).toEqual([{ name: "My dance", characterId: "default" }]);
    expect(result.completed).toEqual({ assetId: `asset-${result.uploadId}`, jobId: `job-${result.uploadId}` });
    expect(upload.state).toMatchObject({ phase: "done", assetId: `asset-${result.uploadId}` });
    expect(progress(upload.state)).toBe(1);
    // Nothing left to resume.
    expect(store.entries.size).toBe(0);
  });

  it("never has more than `concurrency` parts in flight", async () => {
    const { server, upload } = setup(200, { concurrency: 4 });
    await upload.start();
    expect(server.puts).toHaveLength(20);
    expect(server.maxInFlight).toBe(4);
  });

  it("asks for presigned URLs in batches, not one request per part", async () => {
    const { server, upload } = setup(200, { concurrency: 2, presignBatch: 8 });
    await upload.start();
    expect(server.presignCalls.length).toBeLessThanOrEqual(5); // 20 parts
    expect(Math.max(...server.presignCalls.map((batch) => batch.length))).toBe(8);
    // Every part's URL was requested exactly once.
    expect(server.presignCalls.flat().toSorted((a, b) => a - b)).toEqual(range(1, 20));
  });

  it("reports progress that only moves forward", async () => {
    const { upload } = setup(95);
    const seen: number[] = [];
    upload.subscribe((state) => seen.push(progress(state)));
    await upload.start();
    expect(seen.at(-1)).toBe(1);
    for (let i = 1; i < seen.length; i++) expect(seen[i]!).toBeGreaterThanOrEqual(seen[i - 1]! - 1e-9);
  });

  it("saves the upload id as soon as the upload exists", async () => {
    const { server, store, file, upload } = setup(95);
    let savedDuringUpload: string | undefined;
    server.onPut = () => {
      savedDuringUpload ??= store.load(fingerprint(file))?.uploadId;
    };
    const result = await upload.start();
    expect(savedDuringUpload).toBe(result.uploadId);
  });
});

describe("retries", () => {
  it("retries a failing part with growing backoff and leaves the others alone", async () => {
    const { server, delays, upload } = setup(50);
    server.onPut = (part, attempt) => {
      if (part === 3 && attempt <= 2) throw new Error("connection reset");
    };

    const result = await upload.start();

    expect(server.putAttempts.filter((p) => p === 3)).toHaveLength(3);
    expect(server.putCounts()).toEqual({ 1: 1, 2: 1, 3: 1, 4: 1, 5: 1 });
    expect(delays).toEqual([100, 200]); // backoffMs(1), backoffMs(2)
    expect(result.completed).not.toBeNull();
    expect(upload.state.parts[2]!.attempts).toBe(3);
  });

  it("requests a new URL when the bucket rejects one (it may have expired)", async () => {
    const { server, upload } = setup(30);
    server.onPut = (part, attempt) => {
      if (part === 2 && attempt === 1) throw new PartRejected(403);
    };
    await upload.start();
    const timesPresigned = server.presignCalls.flat().filter((p) => p === 2).length;
    expect(timesPresigned).toBe(2);
  });

  it("stops as interrupted after maxAttempts, keeping what was uploaded", async () => {
    const { server, store, file, upload } = setup(50, { maxAttempts: 3, concurrency: 1 });
    server.onPut = (part) => {
      if (part === 3) throw new Error("offline");
    };

    await expect(upload.start()).rejects.toEqual(new UploadStopped("interrupted"));

    expect(upload.state.phase).toBe("interrupted");
    expect(upload.state.error).toBe("offline");
    expect(server.putAttempts.filter((p) => p === 3)).toHaveLength(3);
    // Parts 1 and 2 are safely on the server and the upload can be resumed.
    expect([...server.uploads.values()][0]!.recorded.size).toBe(2);
    expect(store.load(fingerprint(file))?.completedParts).toEqual([1, 2]);
    expect(server.completeCalls).toHaveLength(0);
  });

  it("resumes an interrupted upload without resending finished parts", async () => {
    const { server, upload } = setup(50, { maxAttempts: 2, concurrency: 1 });
    let online = false;
    server.onPut = (part) => {
      if (part >= 3 && !online) throw new Error("offline");
    };
    await expect(upload.start()).rejects.toBeInstanceOf(UploadStopped);

    online = true;
    const result = await upload.resume();

    expect(result.skippedParts).toEqual([1, 2]);
    expect(result.sentParts).toEqual([3, 4, 5]);
    expect(server.putCounts()).toEqual({ 1: 1, 2: 1, 3: 1, 4: 1, 5: 1 });
    expect(server.createCalls).toBe(1); // same upload, not a new one
    expect(upload.state.phase).toBe("done");
  });

  it("does not send a part whose URL arrived after the upload was stopped", async () => {
    // Part 1 fails at once and interrupts the upload while part 2 is still
    // waiting for its presigned URL. When that URL finally arrives the part
    // must not be sent: nobody would record it, and the next resume would
    // find a part on the server that this run never reported.
    const { server, upload } = setup(30, { maxAttempts: 1, concurrency: 2, presignBatch: 1 });
    server.onPut = (part) => {
      if (part === 1) throw new Error("offline");
    };
    let releaseUrl!: () => void;
    const urlHeld = new Promise<void>((release) => (releaseUrl = release));
    const presign = server.presignParts.bind(server);
    server.presignParts = async (id, parts) => {
      if (parts.includes(2)) await urlHeld;
      return presign(id, parts);
    };

    const run = upload.start();
    await expect(run).rejects.toEqual(new UploadStopped("interrupted"));
    expect(server.putAttempts).toEqual([1]);

    releaseUrl();
    await new Promise((r) => setTimeout(r, 10)); // let the waiting worker wake up and settle
    expect(server.putAttempts).toEqual([1]); // part 2 was never started
    expect(upload.state.phase).toBe("interrupted");
  });
});

describe("resuming in a new session", () => {
  /** Run an upload that dies (is abandoned) once `count` parts are on the server. */
  async function crashAfter(ctx: ReturnType<typeof setup>, count: number) {
    const first = ctx.make({ concurrency: 1 });
    ctx.server.onPut = (part) => {
      if (part > count) first.pause(); // stands in for the tab closing
    };
    await expect(first.start()).rejects.toBeInstanceOf(UploadStopped);
    ctx.server.onPut = () => undefined;
  }

  it("sends only the parts the server does not have", async () => {
    const ctx = setup(95);
    await crashAfter(ctx, 4);
    expect(ctx.server.putCounts()).toEqual({ 1: 1, 2: 1, 3: 1, 4: 1 });

    // A brand new engine: same file, same storage, no memory of the first.
    const second = ctx.make();
    const result = await second.start();

    expect(result.skippedParts).toEqual([1, 2, 3, 4]);
    expect(result.sentParts.toSorted((a, b) => a - b)).toEqual(range(5, 10));
    // The proof: no part reached the bucket twice.
    expect(Object.values(ctx.server.putCounts())).toEqual(Array(10).fill(1));
    expect(ctx.server.createCalls).toBe(1);
    expect(second.state.parts.filter((p) => p.resumed).map((p) => p.number)).toEqual([1, 2, 3, 4]);
  });

  it("trusts the server over local storage", async () => {
    const ctx = setup(50);
    await crashAfter(ctx, 3);
    // Local storage is wrong in both directions: it claims part 5 (never
    // sent) and has lost part 2 (which is on the server).
    const key = fingerprint(ctx.file);
    ctx.store.save(key, { ...ctx.store.load(key)!, completedParts: [1, 3, 5] });

    const result = await ctx.make().start();

    expect(result.skippedParts).toEqual([1, 2, 3]);
    expect(result.sentParts.toSorted((a, b) => a - b)).toEqual([4, 5]);
  });

  it("does not resend a part that reached the bucket but was never reported", async () => {
    const ctx = setup(50);
    const first = ctx.make({ concurrency: 1, maxAttempts: 1 });
    // Part 3's bytes arrive, then the client dies before reporting the ETag.
    ctx.server.failRecord = (part) => part === 3;
    await expect(first.start()).rejects.toBeInstanceOf(UploadStopped);
    const upload = [...ctx.server.uploads.values()][0]!;
    expect(upload.bucket.has(3)).toBe(true);
    expect(upload.recorded.has(3)).toBe(false);

    ctx.server.failRecord = () => false;
    const result = await ctx.make().start();

    // Reconcile adopted part 3 from the bucket.
    expect(result.skippedParts).toEqual([1, 2, 3]);
    expect(ctx.server.putCounts()[3]).toBe(1);
  });

  it("starts over when the saved upload was aborted on the server", async () => {
    const ctx = setup(50);
    await crashAfter(ctx, 2);
    const stale = [...ctx.server.uploads.values()][0]!;
    stale.status = "aborted"; // e.g. the reaper cleaned it up after 24 hours

    const result = await ctx.make().start();

    expect(ctx.server.createCalls).toBe(2);
    expect(result.uploadId).not.toBe(stale.id);
    expect(result.skippedParts).toEqual([]);
    expect(result.sentParts).toHaveLength(5);
  });

  it("starts over when the saved upload no longer exists", async () => {
    const ctx = setup(50);
    await crashAfter(ctx, 2);
    ctx.server.uploads.clear();

    const result = await ctx.make().start();
    expect(result.sentParts).toHaveLength(5);
  });

  it("only fetches the result when the saved upload already completed", async () => {
    const ctx = setup(50);
    const first = ctx.make();
    const done = await first.start();
    // Pretend the first session never saw the response to complete.
    ctx.store.save(fingerprint(ctx.file), {
      uploadId: done.uploadId, fileName: "clip.mp4", fileSize: 50, partCount: 5, completedParts: [1, 2, 3, 4, 5], updatedAt: 0,
    });
    const putsBefore = ctx.server.puts.length;

    const result = await ctx.make().start();

    expect(ctx.server.puts.length).toBe(putsBefore);
    expect(result.sentParts).toEqual([]);
    expect(result.completed).toEqual(done.completed);
  });

  it("treats a different file with the same name as a new upload", async () => {
    const ctx = setup(50);
    await crashAfter(ctx, 2);
    const other = new ResumableUpload(fakeFile(70), { api: ctx.server, transport: ctx.server, store: ctx.store });
    const result = await other.start();
    expect(ctx.server.createCalls).toBe(2);
    expect(result.sentParts).toHaveLength(7);
  });
});

describe("pause, resume and cancel", () => {
  it("pauses mid-flight and resumes without resending finished parts", async () => {
    const { server, upload } = setup(95, { concurrency: 2 });
    server.onPut = (part) => {
      if (part === 5) upload.pause();
    };

    await expect(upload.start()).rejects.toEqual(new UploadStopped("paused"));

    expect(upload.state.phase).toBe("paused");
    // Nothing is left marked as uploading, and no half-sent bytes are counted.
    expect(upload.state.parts.some((p) => p.status === "uploading")).toBe(false);
    const finished = upload.state.parts.filter((p) => p.status === "done").length;
    expect(finished).toBeGreaterThan(0);
    expect(finished).toBeLessThan(10);
    expect(server.completeCalls).toHaveLength(0);

    server.onPut = () => undefined;
    const result = await upload.resume();

    expect(result.completed).not.toBeNull();
    expect(Object.values(server.putCounts())).toEqual(Array(10).fill(1));
  });

  it("cancel aborts the upload on the server and forgets it", async () => {
    const { server, store, upload } = setup(95, { concurrency: 2 });
    server.onPut = async (part) => {
      if (part === 3) await upload.cancel();
    };

    await expect(upload.start()).rejects.toEqual(new UploadStopped("cancelled"));

    expect(upload.state.phase).toBe("cancelled");
    expect([...server.uploads.values()][0]!.status).toBe("aborted");
    expect(store.entries.size).toBe(0);
  });
});

describe("splitting upload and completion", () => {
  it("can upload all parts now and complete in a later session", async () => {
    const ctx = setup(50);
    const partsOnly = await ctx.make({ complete: false }).start();
    expect(partsOnly.completed).toBeNull();
    expect(partsOnly.sentParts).toHaveLength(5);
    expect(ctx.server.completeCalls).toHaveLength(0);

    const finish = await ctx.make({ characterId: "none" }).start();
    expect(finish.sentParts).toEqual([]);
    expect(finish.skippedParts).toEqual([1, 2, 3, 4, 5]);
    expect(ctx.server.completeCalls).toEqual([{ name: undefined, characterId: "none" }]);
    expect(ctx.server.puts).toHaveLength(5);
  });
});

describe("fatal errors", () => {
  it("stops without retrying when the server says the upload is gone", async () => {
    const { server, delays, upload } = setup(50, { concurrency: 1 });
    server.onPut = (part) => {
      if (part === 2) [...server.uploads.values()][0]!.status = "aborted";
    };

    await expect(upload.start()).rejects.toBeInstanceOf(ApiError);

    expect(upload.state.phase).toBe("error");
    expect(delays).toEqual([]);
  });

  it("surfaces a rejected create (for example a file that is too large)", async () => {
    const { server, upload } = setup(50);
    server.createUpload = async () => {
      throw new ApiError(413, "too_large", "file exceeds the maximum upload size");
    };
    await expect(upload.start()).rejects.toMatchObject({ code: "too_large" });
    expect(upload.state).toMatchObject({ phase: "error", error: "file exceeds the maximum upload size" });
  });
});
