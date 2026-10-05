import { expect, test } from "@playwright/test";
import { execFileSync } from "node:child_process";
import { mkdirSync, rmSync } from "node:fs";
import { resolve } from "node:path";
import { REPO, SAMPLE, SCREENSHOT_DIR, signIn } from "./helpers";

// Not a test of the app: this records the animated preview for the README.
// It only runs with SCREENSHOTS=1, against a running stack, and writes
// docs/screenshots/preview.gif from real screenshots of the preview page.
test("record the preview as a GIF", async ({ page }) => {
  test.skip(!SCREENSHOT_DIR, "set SCREENSHOTS=1 to record");
  await signIn(page, "docs");
  await page.goto("/upload");
  await page.getByTestId("video-file").setInputFiles(SAMPLE);
  await page.getByLabel("Asset name").fill("Power jump");
  await page.getByTestId("create-motion").click();

  const view = page.getByTestId("motion-view");
  await expect(view).toHaveAttribute("data-loaded", "true", { timeout: 180_000 });
  await page.getByTestId("play-toggle").click(); // pause; frames are stepped by hand

  // Both views (motion and source video) are siblings inside one container.
  const both = view.locator("xpath=..");
  const frames = resolve(REPO, "tmp/preview-frames");
  rmSync(frames, { recursive: true, force: true });
  mkdirSync(frames, { recursive: true });

  const fps = 10;
  const duration = 5.2;
  const scrubber = page.getByLabel("Position");
  const video = page.getByTestId("source-video");
  for (let i = 0; i < duration * fps; i++) {
    const time = i / fps;
    // Multiples of 0.1 s are whole frames at 30 fps, so the range input
    // accepts them as they are ("0.3", not "0.300").
    await scrubber.fill(String(Math.round(time * 10) / 10));
    // Wait until the video has actually shown the frame it was asked for.
    await expect.poll(() => video.evaluate((v: HTMLVideoElement) => !v.seeking && v.readyState >= 2)).toBe(true);
    await page.waitForTimeout(120);
    await both.screenshot({ path: resolve(frames, `${String(i).padStart(3, "0")}.png`) });
  }

  // Two passes: build a palette from the frames, then encode with it. That is
  // what keeps a GIF of a 3D scene from banding.
  const gif = resolve(SCREENSHOT_DIR!, "preview.gif");
  const palette = resolve(frames, "palette.png");
  const input = ["-framerate", String(fps), "-i", resolve(frames, "%03d.png")];
  execFileSync("ffmpeg", ["-y", "-loglevel", "error", ...input, "-vf", "scale=880:-1:flags=lanczos,palettegen=max_colors=128", palette]);
  execFileSync("ffmpeg", [
    "-y", "-loglevel", "error", ...input, "-i", palette,
    "-lavfi", "scale=880:-1:flags=lanczos[x];[x][1:v]paletteuse=dither=bayer:bayer_scale=4",
    gif,
  ]); // prettier-ignore

  // Leave nothing behind in the deployment.
  await page.getByRole("link", { name: "Export" }).click();
  await page.getByTestId("delete-asset").click();
  await page.getByTestId("confirm-delete").click();
  await expect(page.getByRole("heading", { name: "Assets" })).toBeVisible();
});
