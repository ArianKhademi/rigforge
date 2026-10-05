import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import type { Asset } from "../api/types";
import { makeAsset, makeJob, renderWithApp } from "../test-utils";
import { BrowsePage } from "./BrowsePage";

function card(name: string): HTMLElement {
  return screen.getByRole("link", { name });
}

describe("browse grid", () => {
  it("shows a card per asset with poster, name, duration and status", async () => {
    const listAssets = vi.fn(async () => [
      makeAsset({ id: "a", name: "Power jump" }),
      makeAsset({ id: "b", name: "Walk cycle", durationSeconds: 83, urls: { poster: "https://bucket.test/b.jpg" } }),
    ]);
    renderWithApp(<BrowsePage />, { listAssets });

    const jump = await screen.findByRole("link", { name: "Power jump" });
    expect(jump).toHaveAttribute("href", "/assets/a");
    expect(within(jump).getByText("0:05")).toBeInTheDocument();
    expect(within(jump).getByTestId("status-badge")).toHaveTextContent("Ready");
    expect(jump.querySelector("img")).toHaveAttribute("src", "https://bucket.test/assets/a/poster.jpg");

    expect(within(card("Walk cycle")).getByText("1:23")).toBeInTheDocument();
    expect(screen.getAllByTestId("asset-card")).toHaveLength(2);
  });

  it("shows the stage and percent of a job that is running", async () => {
    const processing = makeAsset({
      name: "Dance",
      status: "processing",
      durationSeconds: null,
      urls: {},
      job: makeJob({ status: "extracting", progress: 47, finishedAt: null }),
    });
    renderWithApp(<BrowsePage />, { listAssets: async () => [processing] });

    const dance = await screen.findByRole("link", { name: "Dance" });
    expect(within(dance).getByTestId("status-badge")).toHaveTextContent("Extracting pose 47%");
    // No poster yet, and no retry or error text on a first attempt.
    expect(dance.querySelector("img")).toBeNull();
    expect(within(dance).queryByTestId("retry-count")).toBeNull();
    expect(within(dance).queryByTestId("job-error")).toBeNull();
  });

  it("shows the retry count and the last error while a retry is pending", async () => {
    const retrying = makeAsset({
      name: "Flaky",
      status: "processing",
      urls: {},
      job: makeJob({
        status: "queued",
        progress: 0,
        attempt: 2,
        error: "ConnectionError: bucket unreachable",
        retryAt: new Date(Date.now() + 60_000).toISOString(),
      }),
    });
    renderWithApp(<BrowsePage />, { listAssets: async () => [retrying] });

    const flaky = await screen.findByRole("link", { name: "Flaky" });
    expect(within(flaky).getByTestId("status-badge")).toHaveTextContent("Waiting to retry");
    expect(within(flaky).getByTestId("retry-count")).toHaveTextContent(
      "Retry 1 of 2: ConnectionError: bucket unreachable",
    );
  });

  it("shows the error text of a failed job and how many attempts were made", async () => {
    const failed = makeAsset({
      name: "Empty room",
      status: "failed",
      urls: {},
      job: makeJob({ status: "failed", progress: 30, attempt: 3, error: "no person was detected in the video" }),
    });
    renderWithApp(<BrowsePage />, { listAssets: async () => [failed] });

    const room = await screen.findByRole("link", { name: "Empty room" });
    expect(within(room).getByTestId("status-badge")).toHaveTextContent("Failed");
    const error = within(room).getByTestId("job-error");
    expect(error).toHaveTextContent("no person was detected in the video");
    expect(error).toHaveTextContent("Gave up after attempt 3 of 3");
  });

  it("searches on the server, debounced, and keeps the sort", async () => {
    const user = userEvent.setup();
    const listAssets = vi.fn(async (query: { q?: string }) =>
      [makeAsset({ id: "a", name: "Power jump" }), makeAsset({ id: "b", name: "Walk cycle" })].filter((a) =>
        a.name.toLowerCase().includes((query.q ?? "").toLowerCase()),
      ),
    );
    renderWithApp(<BrowsePage />, { listAssets });
    await screen.findByRole("link", { name: "Walk cycle" });
    expect(listAssets).toHaveBeenLastCalledWith({ q: "", sort: "newest" }, expect.anything());

    await user.selectOptions(screen.getByLabelText("Sort"), "name");
    await waitFor(() => expect(listAssets).toHaveBeenLastCalledWith({ q: "", sort: "name" }, expect.anything()));

    const callsBefore = listAssets.mock.calls.length;
    await user.type(screen.getByLabelText("Search"), "walk");
    await waitFor(() => expect(screen.queryByRole("link", { name: "Power jump" })).toBeNull());
    expect(card("Walk cycle")).toBeInTheDocument();
    expect(listAssets).toHaveBeenLastCalledWith({ q: "walk", sort: "name" }, expect.anything());
    // Four keystrokes, one request: typing is debounced.
    expect(listAssets.mock.calls.length).toBe(callsBefore + 1);
  });

  it("explains an empty library and an empty search differently", async () => {
    const user = userEvent.setup();
    renderWithApp(<BrowsePage />, { listAssets: async (): Promise<Asset[]> => [] });

    expect(await screen.findByText("No assets yet.")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Upload a video" })).toHaveAttribute("href", "/upload");

    await user.type(screen.getByLabelText("Search"), "zzz");
    expect(await screen.findByText("No assets match “zzz”.")).toBeInTheDocument();
  });

  it("reports a failed request instead of an empty grid", async () => {
    renderWithApp(<BrowsePage />, {
      listAssets: async () => {
        throw new Error("503 Service Unavailable");
      },
    });
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not load assets: 503 Service Unavailable");
  });
});
