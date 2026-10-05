import { expect, test, type Request } from "@playwright/test";
import { mkdirSync, writeFileSync } from "node:fs";
import { resolve } from "node:path";
import { bigVideo, SCREENSHOT_DIR, shot, signIn, token, watchForErrors } from "./helpers";

// Resumable uploads in the browser, with a real multi-part file (about 380 MiB,
// six 64 MiB parts) and real requests to the bucket. The connection is "cut"
// by aborting the PUTs for the second half of the parts.

const PARTS = 6;
const partOf = (request: Request) => Number(new URL(request.url()).searchParams.get("partNumber"));
const isPartPut = (request: Request) => request.method() === "PUT" && new URL(request.url()).searchParams.has("partNumber");

test("a dropped connection interrupts the upload and resume sends only the missing parts", async ({ page }) => {
  const assertNoErrors = watchForErrors(page);
  const file = bigVideo(380);
  await signIn(page, "e2e-resume");

  // Every part that fully reached the bucket, in order, for the whole test.
  const delivered: number[] = [];
  page.on("requestfinished", (request) => {
    if (isPartPut(request)) delivered.push(partOf(request));
  });

  // Cut the connection for parts 4 to 6.
  let connectionDown = true;
  await page.route(
    (url) => url.searchParams.has("partNumber"),
    (route) => (connectionDown && partOf(route.request()) > 3 ? route.abort("connectionfailed") : route.continue()),
  );

  await page.goto("/upload");
  await page.getByTestId("video-file").setInputFiles(file);
  await expect(page.getByText(`${PARTS} parts`)).toBeVisible();
  await page.getByLabel("Asset name").fill("Resume demo");
  await page.getByTestId("create-motion").click();

  const progress = page.getByTestId("upload-progress");
  await expect(progress).toBeVisible();
  await shot(page, "resume-1-uploading");

  // The client retries each failing part with backoff, then stops and says so.
  await expect(progress).toHaveAttribute("data-phase", "interrupted", { timeout: 60_000 });
  await expect(progress.getByRole("alert")).toContainText("3 of 6 parts are safely uploaded");
  await expect(page.getByTestId("parts-done")).toHaveText("3 of 6 parts");
  expect([...delivered].sort()).toEqual([1, 2, 3]);
  await shot(page, "resume-2-connection-lost");

  // What the server knows at this point, straight from the api: the recorded
  // parts and the bucket's own part list.
  const uploadId = await page.evaluate(() => {
    const key = Object.keys(window.localStorage).find((k) => k.startsWith("rigforge.upload."))!;
    return JSON.parse(window.localStorage.getItem(key)!).uploadId as string;
  });
  const response = await page.request.get(`/api/uploads/${uploadId}?storage=true`, {
    headers: { Authorization: `Bearer ${await token(page)}` },
  });
  const serverState = await response.json();
  expect(serverState.status).toBe("uploading");
  expect(serverState.parts.map((p: { partNumber: number }) => p.partNumber)).toEqual([1, 2, 3]);
  expect(serverState.storageParts.map((p: { partNumber: number }) => p.partNumber)).toEqual([1, 2, 3]);
  if (SCREENSHOT_DIR) {
    mkdirSync(SCREENSHOT_DIR, { recursive: true });
    writeFileSync(resolve(SCREENSHOT_DIR, "resume-part-list.json"), JSON.stringify(serverState, null, 2) + "\n");
  }

  // The connection comes back; the user resumes.
  connectionDown = false;
  const before = delivered.length;
  await page.getByRole("button", { name: "Resume" }).click();
  await expect(page.getByTestId("resumed-note")).toContainText("3 parts were already on the server");
  await shot(page, "resume-3-resumed");

  await page.waitForURL(/\/assets\/[0-9a-f-]{36}$/, { timeout: 120_000 });
  // After the resume only the three missing parts were sent...
  expect(delivered.slice(before).sort()).toEqual([4, 5, 6]);
  // ...so over the whole upload every part reached the bucket exactly once.
  expect([...delivered].sort()).toEqual([1, 2, 3, 4, 5, 6]);

  // The assembled object is the full file.
  await expect(page.getByRole("heading", { name: "Resume demo" })).toBeVisible();
  await expect(page.getByText(/38\d MiB source/)).toBeVisible();

  // Clean up the 380 MiB object (also stops the worker's job for it).
  await page.getByTestId("delete-asset").click();
  await page.getByTestId("confirm-delete").click();
  await expect(page.getByRole("heading", { name: "Assets" })).toBeVisible();
  assertNoErrors();
});

test("an upload survives a page reload and continues from the server's part list", async ({ page }) => {
  const assertNoErrors = watchForErrors(page);
  const file = bigVideo(380);
  await signIn(page, "e2e-reload");

  const delivered: number[] = [];
  page.on("requestfinished", (request) => {
    if (isPartPut(request)) delivered.push(partOf(request));
  });
  // Hold parts 3 to 6 back so the reload happens with the upload half done.
  let hold = true;
  const held: (() => void)[] = [];
  await page.route(
    (url) => url.searchParams.has("partNumber"),
    async (route) => {
      if (hold && partOf(route.request()) > 2) await new Promise<void>((release) => held.push(release));
      await route.continue().catch(() => undefined); // the page may be gone by now
    },
  );

  await page.goto("/upload");
  await page.getByTestId("video-file").setInputFiles(file);
  await page.getByTestId("create-motion").click();
  await expect(page.getByTestId("parts-done")).toHaveText("2 of 6 parts", { timeout: 60_000 });

  // Reload mid-upload: the tab's in-flight requests die with it.
  await page.reload();
  hold = false;
  held.forEach((release) => release());
  expect([...delivered].sort()).toEqual([1, 2]);

  // The page remembers the unfinished upload...
  await expect(page.getByRole("region", { name: "Unfinished uploads" })).toContainText("2 of 6 parts sent");
  // ...and picking the same file again offers to resume it.
  await page.getByTestId("video-file").setInputFiles(file);
  await expect(page.getByTestId("resume-hint")).toContainText("2 of 6 parts sent");
  const before = delivered.length;
  await page.getByTestId("create-motion").click();
  await expect(page.getByTestId("resumed-note")).toContainText("2 parts were already on the server");

  await page.waitForURL(/\/assets\/[0-9a-f-]{36}$/, { timeout: 120_000 });
  expect(delivered.slice(before).sort()).toEqual([3, 4, 5, 6]);
  expect([...delivered].sort()).toEqual([1, 2, 3, 4, 5, 6]);
  // Nothing is left to resume on this device.
  expect(await page.evaluate(() => Object.keys(window.localStorage).filter((k) => k.startsWith("rigforge.upload.")))).toEqual([]);

  await page.getByTestId("delete-asset").click();
  await page.getByTestId("confirm-delete").click();
  await expect(page.getByRole("heading", { name: "Assets" })).toBeVisible();
  assertNoErrors();
});
