import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// In development the Vite server proxies the api and the dev issuer, so the
// browser talks to a single origin just as it does behind nginx or the ingress.
const API = process.env.RIGFORGE_API ?? "http://localhost:8080";
const ISSUER = process.env.RIGFORGE_ISSUER ?? "http://localhost:8090";

export default defineConfig({
  plugins: [react()],
  server: {
    port: Number(process.env.PORT ?? 5173),
    proxy: {
      "/api": { target: API, changeOrigin: true },
      "/issuer": { target: ISSUER, changeOrigin: true, rewrite: (path) => path.replace(/^\/issuer/, "") },
    },
  },
  build: {
    // Vite's default output folder is "assets", which is also an app route
    // (/assets/:id). Keeping built files under /static avoids the clash and
    // lets nginx cache that folder aggressively.
    assetsDir: "static",
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test-setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
    css: { modules: { classNameStrategy: "non-scoped" } },
  },
});
