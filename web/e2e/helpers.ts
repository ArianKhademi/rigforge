import { expect, type Page } from "@playwright/test";
import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, statSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const REPO = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
export const SAMPLE = resolve(REPO, "docs/samples/power_jump.mp4");
/** Set SCREENSHOTS=1 to (re)write the images the README uses. */
export const SCREENSHOT_DIR = process.env.SCREENSHOTS ? resolve(REPO, "docs/screenshots") : null;

/**
 * Fail the test on anything the app logs as an error: uncaught exceptions,
 * unhandled promise rejections and console.error calls. Requests the test
 * itself aborts (to simulate a dropped connection) are reported by the
 * browser as failed resource loads; those are expected and filtered out.
 */
export function watchForErrors(page: Page): () => void {
  const problems: string[] = [];
  page.on("pageerror", (error) => problems.push(`uncaught: ${error.message}`));
  page.on("console", (message) => {
    if (message.type() !== "error") return;
    const text = message.text();
    if (text.includes("Failed to load resource") || text.includes("net::ERR_")) return;
    problems.push(`console.error: ${text}`);
  });
  return () => expect(problems, "the app logged errors").toEqual([]);
}

export async function signIn(page: Page, prefix: string): Promise<string> {
  const user = `${prefix}-${Date.now().toString(36)}`;
  await page.goto("/login");
  await page.getByLabel("User name").fill(user);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Assets" })).toBeVisible();
  return user;
}

/** The signed-in user's token, for calling the api directly from a test. */
export async function token(page: Page): Promise<string> {
  return page.evaluate(() => JSON.parse(window.localStorage.getItem("rigforge.session")!).token as string);
}

export async function shot(page: Page, name: string): Promise<void> {
  if (!SCREENSHOT_DIR) return;
  mkdirSync(SCREENSHOT_DIR, { recursive: true });
  await page.screenshot({ path: resolve(SCREENSHOT_DIR, `${name}.png`) });
}

/** A real multi-part video (about `megabytes` MB), generated once and cached under tmp/. */
export function bigVideo(megabytes: number): string {
  const path = resolve(REPO, `tmp/e2e_${megabytes}mb.mov`);
  const bytes = megabytes * 1024 * 1024;
  if (!existsSync(path) || statSync(path).size < bytes) {
    execFileSync(resolve(REPO, "scripts/make_sample_video.sh"), ["big", path, String(bytes)], { stdio: "inherit" });
  }
  return path;
}

export async function expectNoHorizontalScroll(page: Page): Promise<void> {
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  expect(overflow, "page is wider than the viewport").toBeLessThanOrEqual(0);
}
