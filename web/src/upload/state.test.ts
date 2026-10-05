import { describe, expect, it } from "vitest";
import {
  countParts,
  initialState,
  partLength,
  progress,
  reduce,
  remainingParts,
  uploadedBytes,
  type UploadEvent,
  type UploadState,
} from "./state";

// A 25-byte file in 10-byte parts: three parts of 10, 10 and 5 bytes.
const prepared: UploadEvent = { type: "prepared", uploadId: "u1", partSize: 10, partCount: 3, completed: [] };

function run(events: UploadEvent[], from: UploadState = initialState("clip.mp4", 25)): UploadState {
  return events.reduce(reduce, from);
}

describe("part plan", () => {
  it("gives every part the part size except the last", () => {
    expect([1, 2, 3].map((n) => partLength(25, 10, 3, n))).toEqual([10, 10, 5]);
    // The resume test's file: 2.1 GiB in 64 MiB parts.
    const size = 2254857830;
    const partSize = 64 * 1024 * 1024;
    expect(partLength(size, partSize, 34, 1)).toBe(partSize);
    expect(partLength(size, partSize, 34, 34)).toBe(size - 33 * partSize);
  });

  it("lays out pending parts when a new upload is prepared", () => {
    const state = run([{ type: "preparing" }, prepared]);
    expect(state.phase).toBe("uploading");
    expect(state.uploadId).toBe("u1");
    expect(state.parts.map((p) => [p.number, p.size, p.status])).toEqual([
      [1, 10, "pending"],
      [2, 10, "pending"],
      [3, 5, "pending"],
    ]);
    expect(remainingParts(state)).toEqual([1, 2, 3]);
  });

  it("marks parts the server already has as done when resuming", () => {
    const state = run([{ ...prepared, completed: [3, 1] }]);
    expect(state.parts.map((p) => p.status)).toEqual(["done", "pending", "done"]);
    expect(state.parts.map((p) => p.resumed)).toEqual([true, false, true]);
    expect(remainingParts(state)).toEqual([2]);
    // Their bytes count as uploaded from the start.
    expect(uploadedBytes(state)).toBe(15);
    expect(progress(state)).toBeCloseTo(0.6);
  });
});

describe("part lifecycle", () => {
  it("tracks a part from start through progress to done", () => {
    let state = run([prepared, { type: "part_started", part: 2 }]);
    expect(state.parts[1]).toMatchObject({ status: "uploading", loaded: 0, attempts: 1 });

    state = reduce(state, { type: "part_progress", part: 2, loaded: 6 });
    expect(state.parts[1]!.loaded).toBe(6);
    expect(uploadedBytes(state)).toBe(6);

    state = reduce(state, { type: "part_done", part: 2 });
    expect(state.parts[1]).toMatchObject({ status: "done", loaded: 10 });
  });

  it("accepts parts finishing out of order", () => {
    const state = run([
      prepared,
      { type: "part_started", part: 1 },
      { type: "part_started", part: 2 },
      { type: "part_started", part: 3 },
      { type: "part_done", part: 3 },
      { type: "part_done", part: 1 },
    ]);
    expect(state.parts.map((p) => p.status)).toEqual(["done", "uploading", "done"]);
    expect(countParts(state, "done")).toBe(2);
  });

  it("never reports more bytes than a part holds", () => {
    const state = run([prepared, { type: "part_started", part: 3 }, { type: "part_progress", part: 3, loaded: 999 }]);
    expect(state.parts[2]!.loaded).toBe(5);
  });

  it("ignores stale progress for a part that is not uploading", () => {
    const state = run([
      prepared,
      { type: "part_started", part: 1 },
      { type: "part_done", part: 1 },
      { type: "part_progress", part: 1, loaded: 3 }, // late event from the finished request
      { type: "part_progress", part: 2, loaded: 4 }, // part 2 was never started
    ]);
    expect(state.parts[0]!.loaded).toBe(10);
    expect(state.parts[1]!.loaded).toBe(0);
  });

  it("discards the bytes of a failed attempt and counts the retry", () => {
    const state = run([
      prepared,
      { type: "part_started", part: 1 },
      { type: "part_progress", part: 1, loaded: 7 },
      { type: "part_retry", part: 1, error: "network error" },
    ]);
    expect(state.parts[0]).toMatchObject({ status: "pending", loaded: 0, attempts: 1 });
    expect(state.phase).toBe("uploading");

    const retried = reduce(state, { type: "part_started", part: 1 });
    expect(retried.parts[0]!.attempts).toBe(2);
  });

  it("becomes interrupted, with the error, when a part runs out of retries", () => {
    const state = run([
      prepared,
      { type: "part_started", part: 1 },
      { type: "part_done", part: 1 },
      { type: "part_started", part: 2 },
      { type: "part_failed", part: 2, error: "connection reset" },
    ]);
    expect(state.phase).toBe("interrupted");
    expect(state.error).toBe("connection reset");
    expect(state.parts[1]!.status).toBe("failed");
    // The finished part is kept; the failed one is still to do.
    expect(remainingParts(state)).toEqual([2, 3]);
  });
});

describe("pause", () => {
  const midFlight = run([
    prepared,
    { type: "part_started", part: 1 },
    { type: "part_done", part: 1 },
    { type: "part_started", part: 2 },
    { type: "part_progress", part: 2, loaded: 8 },
  ]);

  it("forgets in-flight progress but keeps completed parts", () => {
    const state = reduce(midFlight, { type: "paused" });
    expect(state.phase).toBe("paused");
    expect(state.parts.map((p) => [p.status, p.loaded])).toEqual([
      ["done", 10],
      ["pending", 0],
      ["pending", 0],
    ]);
  });

  it("does nothing once the upload has finished", () => {
    const done = run([{ type: "completing" }, { type: "completed", assetId: "a", jobId: "j" }], midFlight);
    expect(reduce(done, { type: "paused" })).toBe(done);
  });

  it("keeps the interrupted phase when in-flight parts are reset after a failure", () => {
    const state = run(
      [
        { type: "part_started", part: 3 },
        { type: "part_failed", part: 2, error: "boom" },
        { type: "paused" },
      ],
      midFlight,
    );
    expect(state.phase).toBe("interrupted");
    expect(state.parts[2]!.status).toBe("pending");
  });
});

describe("finishing", () => {
  it("reports full progress and the created asset and job", () => {
    const state = run([
      prepared,
      { type: "part_done", part: 1 },
      { type: "part_done", part: 2 },
      { type: "part_done", part: 3 },
      { type: "completing" },
      { type: "completed", assetId: "asset-1", jobId: "job-1" },
    ]);
    expect(state).toMatchObject({ phase: "done", assetId: "asset-1", jobId: "job-1", error: null });
    expect(progress(state)).toBe(1);
  });

  it("records a fatal error", () => {
    const state = run([prepared, { type: "error", error: "upload was aborted" }]);
    expect(state).toMatchObject({ phase: "error", error: "upload was aborted" });
  });

  it("clears a previous error when the upload is prepared again", () => {
    const state = run([prepared, { type: "part_failed", part: 1, error: "x" }, { type: "preparing" }, prepared]);
    expect(state.error).toBeNull();
    expect(state.phase).toBe("uploading");
  });
});
