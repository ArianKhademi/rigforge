import type {
  Asset,
  AssetSort,
  Character,
  CompletedUpload,
  CreatedUpload,
  ExportLink,
  Job,
  PresignedPart,
  UploadInfo,
} from "./types";

/** An error response from the api, carrying its {code, message, details} envelope. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly details?: unknown,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

export interface ApiOptions {
  /** Origin of the api, e.g. "http://localhost:8080". Empty = same origin. */
  baseUrl: string;
  /** Returns the current bearer token, or null when signed out. */
  getToken: () => string | null;
  /** Called on a 401 so the app can sign the user out. */
  onUnauthorized?: () => void;
  fetch?: typeof fetch;
}

export type Api = ReturnType<typeof createApi>;

/**
 * Typed client for the Rigforge api. It has no browser dependencies, so the
 * same client runs in the web app, in the upload CLI and in tests.
 */
export function createApi(options: ApiOptions) {
  const doFetch = options.fetch ?? fetch;

  async function request<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
    const headers: Record<string, string> = {};
    const token = options.getToken();
    if (token) headers.Authorization = `Bearer ${token}`;

    let payload: BodyInit | undefined;
    if (body instanceof FormData) {
      payload = body; // the runtime sets the multipart boundary itself
    } else if (body !== undefined) {
      headers["Content-Type"] = "application/json";
      payload = JSON.stringify(body);
    }

    const response = await doFetch(options.baseUrl + path, { method, headers, body: payload, signal });
    if (response.status === 401) options.onUnauthorized?.();
    if (!response.ok) {
      // Every api error has the same JSON envelope; anything else (a proxy's
      // HTML error page, say) falls back to the status text.
      const envelope = (await response.json().catch(() => null)) as {
        error?: { code?: string; message?: string; details?: unknown };
      } | null;
      throw new ApiError(
        response.status,
        envelope?.error?.code ?? "http_error",
        envelope?.error?.message ?? `${response.status} ${response.statusText}`,
        envelope?.error?.details,
      );
    }
    if (response.status === 204) return undefined as T;
    return (await response.json()) as T;
  }

  return {
    baseUrl: options.baseUrl,
    getToken: options.getToken,

    me: () => request<{ userId: string }>("GET", "/api/me"),

    // ---- uploads ----
    createUpload: (file: { filename: string; size: number; contentType: string }) =>
      request<CreatedUpload>("POST", "/api/uploads", file),
    getUpload: (id: string, withStorage = false) =>
      request<UploadInfo>("GET", `/api/uploads/${id}${withStorage ? "?storage=true" : ""}`),
    /** Adopts parts that reached the bucket but were never reported, then returns the upload. */
    reconcileUpload: (id: string) => request<UploadInfo>("POST", `/api/uploads/${id}/reconcile`),
    presignParts: async (id: string, partNumbers: number[]) =>
      (await request<{ urls: PresignedPart[] }>("POST", `/api/uploads/${id}/parts`, { partNumbers })).urls,
    recordPart: (id: string, partNumber: number, etag: string) =>
      request<unknown>("PUT", `/api/uploads/${id}/parts/${partNumber}`, { etag }).then(() => undefined),
    completeUpload: (id: string, body: { name?: string; characterId?: string }) =>
      request<CompletedUpload>("POST", `/api/uploads/${id}/complete`, body),
    abortUpload: (id: string) => request<void>("DELETE", `/api/uploads/${id}`),

    // ---- assets ----
    listAssets: async (query: { q?: string; sort?: AssetSort }, signal?: AbortSignal) => {
      const params = new URLSearchParams();
      if (query.q) params.set("q", query.q);
      if (query.sort) params.set("sort", query.sort);
      const suffix = params.size > 0 ? `?${params}` : "";
      return (await request<{ assets: Asset[] }>("GET", `/api/assets${suffix}`, undefined, signal)).assets;
    },
    getAsset: (id: string, signal?: AbortSignal) => request<Asset>("GET", `/api/assets/${id}`, undefined, signal),
    deleteAsset: (id: string) => request<void>("DELETE", `/api/assets/${id}`),
    exportAsset: (id: string, format: "glb" | "bvh") =>
      request<ExportLink>("GET", `/api/assets/${id}/export?format=${format}`),
    reprocessAsset: (id: string, characterId: string) =>
      request<Job>("POST", `/api/assets/${id}/jobs`, { characterId }),

    // ---- jobs ----
    getJob: (id: string) => request<Job>("GET", `/api/jobs/${id}`),

    // ---- characters ----
    listCharacters: async () => (await request<{ characters: Character[] }>("GET", "/api/characters")).characters,
    uploadCharacter: (file: Blob, filename: string, name?: string) => {
      const form = new FormData();
      form.set("file", file, filename);
      if (name) form.set("name", name);
      return request<Character>("POST", "/api/characters", form);
    },
    deleteCharacter: (id: string) => request<void>("DELETE", `/api/characters/${id}`),
  };
}
