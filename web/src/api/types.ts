// Wire types for the Rigforge api (api/internal/*/handler.go).

export type UploadStatus = "uploading" | "completed" | "aborted";

export interface CreatedUpload {
  uploadId: string;
  assetId: string;
  partSize: number;
  partCount: number;
}

export interface RecordedPart {
  partNumber: number;
  etag: string;
  recordedAt: string;
}

export interface StoredPart {
  partNumber: number;
  etag: string;
  size: number;
  lastModified: string;
}

export interface UploadInfo {
  uploadId: string;
  assetId: string;
  filename: string;
  size: number;
  contentType: string;
  partSize: number;
  partCount: number;
  status: UploadStatus;
  parts: RecordedPart[];
  storageParts?: StoredPart[];
}

export interface PresignedPart {
  partNumber: number;
  url: string;
}

export interface CompletedUpload {
  assetId: string;
  jobId: string;
}

export type JobStatus = "queued" | "transcoding" | "extracting" | "writing" | "uploading" | "done" | "failed";

export interface Job {
  id: string;
  assetId: string;
  type: string;
  characterId: string;
  status: JobStatus;
  progress: number;
  attempt: number;
  maxAttempts: number;
  error: string | null;
  retryAt: string | null;
  createdAt: string;
  updatedAt: string;
  startedAt: string | null;
  finishedAt: string | null;
}

export type AssetStatus = "processing" | "ready" | "failed";

export interface Asset {
  id: string;
  name: string;
  status: AssetStatus;
  sourceSize: number;
  contentType: string;
  durationSeconds: number | null;
  width: number | null;
  height: number | null;
  fps: number | null;
  frameCount: number | null;
  createdAt: string;
  updatedAt: string;
  job: Job | null;
  urls: { poster?: string; preview?: string; motion?: string };
}

export type AssetSort = "newest" | "oldest" | "name" | "duration";

export interface Character {
  id: string;
  name: string;
  builtin: boolean;
  createdAt: string | null;
}

export interface ExportLink {
  url: string;
  filename: string;
  expiresAt: string;
}

export function isTerminal(status: JobStatus): boolean {
  return status === "done" || status === "failed";
}
