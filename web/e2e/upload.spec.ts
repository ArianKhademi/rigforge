import { expect, test } from "@playwright/test";
import { readFileSync } from "node:fs";
import { expectNoHorizontalScroll, SAMPLE, shot, signIn, watchForErrors } from "./helpers";

// The whole product in one pass: upload the sample clip, wait for the worker,
// see the motion play on the character next to the source video, export it.
test("upload the sample clip, preview the motion and export it @mobile", async ({ page }) => {
  const assertNoErrors = watchForErrors(page);
  await signIn(page, "e2e");
  await expectNoHorizontalScroll(page);

  // ---- upload ----
  await page.getByRole("link", { name: "Upload", exact: true }).click();
  await page.getByTestId("video-file").setInputFiles(SAMPLE);
  await expect(page.getByText("power_jump.mp4")).toBeVisible();
  await page.getByLabel("Asset name").fill("Power jump");
  await expect(page.getByRole("radio", { name: /Rigforge Mannequin/ })).toBeChecked();
  await expectNoHorizontalScroll(page);
  await shot(page, "upload-form");
  await page.getByTestId("create-motion").click();

  // ---- processing: the asset page follows the job over server-sent events ----
  await page.waitForURL(/\/assets\/[0-9a-f-]{36}$/);
  await expect(page.getByRole("heading", { name: "Power jump" })).toBeVisible();
  await shot(page, "job-progress");

  // ---- preview ----
  const view = page.getByTestId("motion-view");
  await expect(view).toHaveAttribute("data-loaded", "true", { timeout: 180_000 });
  await expect(page.getByTestId("status-badge")).toHaveText("Ready");
  // The asset contains the character mesh, skinned to the 17 Rigforge joints.
  await expect(view).toHaveAttribute("data-mesh", "true");
  await expect(view).toHaveAttribute("data-bones", "17");
  await expect(view.locator("canvas")).toBeVisible();
  await expect(page.getByText("0:05 · 156 frames")).toBeVisible();
  await expectNoHorizontalScroll(page);

  // The source video loaded and both views are playing on one clock.
  const video = page.getByTestId("source-video");
  await expect.poll(() => video.evaluate((v: HTMLVideoElement) => v.readyState)).toBeGreaterThanOrEqual(2);
  await expect(page.getByTestId("play-toggle")).toHaveText("Pause");
  const playhead = page.getByTestId("playhead");
  const before = await playhead.textContent();
  await expect.poll(() => playhead.textContent()).not.toBe(before);

  // Pause and scrub: the motion view shows a different picture at a different
  // time, i.e. the character really is being animated by the clock.
  await page.getByTestId("play-toggle").click();
  const scrubber = page.getByLabel("Position");
  await scrubber.fill("0.5");
  await expect(playhead).toContainText("0.5 s /");
  expect(await video.evaluate((v: HTMLVideoElement) => v.currentTime)).toBeCloseTo(0.5, 1);
  await page.waitForTimeout(400);
  const squat = await view.locator("canvas").screenshot();
  await scrubber.fill("3.7");
  await expect(playhead).toContainText("3.7 s /");
  await page.waitForTimeout(400);
  const jump = await view.locator("canvas").screenshot();
  expect(squat.equals(jump), "the 3D view did not change between two times").toBe(false);
  await shot(page, "preview");

  await page.getByLabel("Skeleton").check();
  await page.waitForTimeout(300);
  await shot(page, "preview-skeleton");

  // ---- export ----
  await page.getByRole("link", { name: "Export" }).click();
  await expectNoHorizontalScroll(page);
  await shot(page, "export");
  const [glb] = await Promise.all([page.waitForEvent("download"), page.getByTestId("download-glb").click()]);
  expect(glb.suggestedFilename()).toBe("Power jump.glb");
  const glbBytes = readFileSync(await glb.path());
  expect(glbBytes.subarray(0, 4).toString("latin1")).toBe("glTF");
  expect(glbBytes.length).toBeGreaterThan(500_000); // the mannequin mesh alone is about 550 KB

  const [bvh] = await Promise.all([page.waitForEvent("download"), page.getByTestId("download-bvh").click()]);
  expect(bvh.suggestedFilename()).toBe("Power jump.bvh");
  const bvhText = readFileSync(await bvh.path(), "utf8");
  expect(bvhText.startsWith("HIERARCHY\nROOT hips")).toBe(true);
  expect(bvhText).toContain("Frames: 156");

  // ---- browse ----
  await page.getByRole("link", { name: "← Assets" }).click();
  const card = page.getByRole("link", { name: "Power jump" });
  await expect(card.getByTestId("status-badge")).toHaveText("Ready");
  await expect(card.locator("img")).toBeVisible();
  await expectNoHorizontalScroll(page);
  await shot(page, "browse");

  // Search narrows the grid on the server.
  await page.getByLabel("Search").fill("no such asset");
  await expect(page.getByText("No assets match")).toBeVisible();
  await page.getByLabel("Search").fill("power");
  await expect(card).toBeVisible();

  // ---- delete ----
  await card.click();
  await page.getByRole("link", { name: "Export" }).click();
  await page.getByTestId("delete-asset").click();
  await page.getByTestId("confirm-delete").click();
  await expect(page.getByText("No assets yet.")).toBeVisible();

  assertNoErrors();
});

test("a character with the wrong skeleton is rejected with the reason @mobile", async ({ page }) => {
  const assertNoErrors = watchForErrors(page);
  await signIn(page, "e2e-rig");
  await page.goto("/upload");
  await page.getByTestId("video-file").setInputFiles(SAMPLE);

  // A structurally valid GLB with a two-bone skeleton that is not Rigforge's.
  const json = JSON.stringify({
    asset: { version: "2.0" },
    nodes: [{ name: "Body", mesh: 0, skin: 0 }, { name: "mixamorig:Hips", children: [2] }, { name: "mixamorig:Spine" }],
    skins: [{ joints: [1, 2] }],
    meshes: [{ primitives: [{ attributes: { POSITION: 0, JOINTS_0: 1, WEIGHTS_0: 2 } }] }],
  });
  const chunk = Buffer.from(json.padEnd(Math.ceil(json.length / 4) * 4, " "));
  const header = Buffer.alloc(20);
  header.writeUInt32LE(0x46546c67, 0); // "glTF"
  header.writeUInt32LE(2, 4);
  header.writeUInt32LE(20 + chunk.length, 8);
  header.writeUInt32LE(chunk.length, 12);
  header.writeUInt32LE(0x4e4f534a, 16); // "JSON"

  await page.getByTestId("character-file").setInputFiles({
    name: "mixamo.glb",
    mimeType: "model/gltf-binary",
    buffer: Buffer.concat([header, chunk]),
  });

  const error = page.getByTestId("character-error");
  await expect(error).toContainText("skeleton does not match the Rigforge skeleton");
  await expect(error).toContainText("missing joints: hips, spine");
  // The built-in character is still the selection.
  await expect(page.getByRole("radio", { name: /Rigforge Mannequin/ })).toBeChecked();
  await expectNoHorizontalScroll(page);

  // The 4xx for the rejected file is the expected outcome, not an app error.
  assertNoErrors();
});
