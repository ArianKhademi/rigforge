import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { initialState, reduce, type UploadEvent, type UploadState } from "../upload/state";
import { UploadProgress } from "./UploadProgress";

// A 35-byte file in 10-byte parts (4 parts); part 1 was already on the server.
function stateAfter(events: UploadEvent[]): UploadState {
  const prepared: UploadEvent = { type: "prepared", uploadId: "u1", partSize: 10, partCount: 4, completed: [1] };
  return [prepared, ...events].reduce(reduce, initialState("dance.mov", 35));
}

function show(state: UploadState) {
  const handlers = { onPause: vi.fn(), onResume: vi.fn(), onCancel: vi.fn() };
  render(<UploadProgress state={state} speed={2 * 1024 * 1024} {...handlers} />);
  return handlers;
}

function partStatuses(): (string | undefined)[] {
  return within(screen.getByTestId("part-grid"))
    .getAllByRole("listitem")
    .map((cell) => cell.dataset.status);
}

describe("upload progress", () => {
  it("draws one cell per part with its status and counts progress", () => {
    show(
      stateAfter([
        { type: "part_started", part: 2 },
        { type: "part_done", part: 2 },
        { type: "part_started", part: 3 },
        { type: "part_progress", part: 3, loaded: 5 },
      ]),
    );

    expect(partStatuses()).toEqual(["done", "done", "uploading", "pending"]);
    // 10 (resumed) + 10 (done) + 5 (in flight) of 35 bytes.
    expect(screen.getByRole("progressbar")).toHaveAttribute("aria-valuenow", "71");
    expect(screen.getByTestId("parts-done")).toHaveTextContent("2 of 4 parts");
    expect(screen.getByText(/2\.00 MiB\/s/)).toBeInTheDocument();
  });

  it("says which parts were resumed rather than sent again", () => {
    show(stateAfter([]));
    expect(screen.getByTestId("resumed-note")).toHaveTextContent("1 part was already on the server");
  });

  it("offers pause and cancel while uploading", async () => {
    const handlers = show(stateAfter([{ type: "part_started", part: 2 }]));
    await userEvent.click(screen.getByRole("button", { name: "Pause" }));
    await userEvent.click(screen.getByRole("button", { name: "Cancel upload" }));
    expect(handlers.onPause).toHaveBeenCalledOnce();
    expect(handlers.onCancel).toHaveBeenCalledOnce();
    expect(screen.queryByRole("button", { name: "Resume" })).toBeNull();
  });

  it("offers resume when paused", async () => {
    const handlers = show(stateAfter([{ type: "part_started", part: 2 }, { type: "paused" }]));
    expect(screen.getByText("Paused")).toBeInTheDocument();
    expect(partStatuses()).toEqual(["done", "pending", "pending", "pending"]);
    await userEvent.click(screen.getByRole("button", { name: "Resume" }));
    expect(handlers.onResume).toHaveBeenCalledOnce();
  });

  it("explains an interruption and that finished parts are safe", () => {
    show(
      stateAfter([
        { type: "part_started", part: 2 },
        { type: "part_done", part: 2 },
        { type: "part_started", part: 3 },
        { type: "part_failed", part: 3, error: "network error while uploading a part" },
      ]),
    );
    expect(screen.getByText("Connection lost")).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent("2 of 4 parts are safely uploaded");
    expect(partStatuses()).toEqual(["done", "done", "failed", "pending"]);
    expect(screen.getByRole("button", { name: "Resume" })).toBeInTheDocument();
  });

  it("shows a fatal error without a resume button", () => {
    show(stateAfter([{ type: "error", error: "file exceeds the maximum upload size" }]));
    expect(screen.getByRole("alert")).toHaveTextContent("file exceeds the maximum upload size");
    expect(screen.queryByRole("button", { name: "Resume" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Cancel upload" })).toBeNull();
  });
});
