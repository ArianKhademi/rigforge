import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import type { ReactElement } from "react";
import { MemoryRouter } from "react-router-dom";
import type { Api } from "./api/client";
import { ApiProvider } from "./api/context";
import type { Asset, Job } from "./api/types";
import { AuthProvider } from "./auth/auth";

/** Render a component with the providers the app wraps everything in, and a fake api. */
export function renderWithApp(ui: ReactElement, api: Partial<Api>, route = "/") {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <AuthProvider initial={{ token: "test-token", user: "alice", expiresAt: Date.now() + 3_600_000 }}>
        <ApiProvider api={api as Api}>
          <MemoryRouter initialEntries={[route]}>{ui}</MemoryRouter>
        </ApiProvider>
      </AuthProvider>
    </QueryClientProvider>,
  );
}

export function makeJob(overrides: Partial<Job> = {}): Job {
  return {
    id: "job-1",
    assetId: "asset-1",
    type: "process_video",
    characterId: "default",
    status: "done",
    progress: 100,
    attempt: 1,
    maxAttempts: 3,
    error: null,
    retryAt: null,
    createdAt: "2026-10-05T10:00:00Z",
    updatedAt: "2026-10-05T10:00:10Z",
    startedAt: "2026-10-05T10:00:01Z",
    finishedAt: "2026-10-05T10:00:10Z",
    ...overrides,
  };
}

export function makeAsset(overrides: Partial<Asset> = {}): Asset {
  const id = overrides.id ?? "asset-1";
  return {
    id,
    name: "Power jump",
    status: "ready",
    sourceSize: 589_156,
    contentType: "video/mp4",
    durationSeconds: 5.2,
    width: 720,
    height: 720,
    fps: 30,
    frameCount: 156,
    createdAt: new Date(Date.now() - 120_000).toISOString(),
    updatedAt: new Date().toISOString(),
    job: makeJob({ assetId: id }),
    urls: { poster: `https://bucket.test/assets/${id}/poster.jpg` },
    ...overrides,
  };
}
