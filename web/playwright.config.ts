import { defineConfig, devices } from "@playwright/test";

// End-to-end tests run against a full, already running stack (web, api,
// issuer, worker, storage): `make e2e` brings it up with docker-compose and
// then runs this. Point E2E_BASE_URL elsewhere to test the Vite dev server or
// the kind deployment.
export default defineConfig({
  testDir: "./e2e",
  timeout: 240_000,
  expect: { timeout: 15_000 },
  fullyParallel: false,
  workers: 1,
  retries: 0,
  reporter: [["list"]],
  use: {
    baseURL: process.env.E2E_BASE_URL ?? "http://localhost:8081",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
    launchOptions: {
      // Headless Chromium has no GPU; these make it render WebGL (the 3D
      // preview) with its software rasteriser.
      args: ["--use-angle=swiftshader", "--enable-unsafe-swiftshader", "--ignore-gpu-blocklist"],
    },
  },
  projects: [
    { name: "desktop", use: { ...devices["Desktop Chrome"], viewport: { width: 1280, height: 800 } } },
    // The same flows on a phone-sized viewport (390 px wide).
    { name: "mobile", use: { ...devices["Desktop Chrome"], viewport: { width: 390, height: 844 } }, grep: /@mobile/ },
  ],
});
