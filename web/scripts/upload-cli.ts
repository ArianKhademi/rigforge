// Command-line client for resumable uploads.
//
//   pnpm upload <file> --api http://localhost:8080 --token <jwt> [options]
//
// It drives the same ResumableUpload engine the browser uses, with a file on
// disk instead of a File object and a JSON file instead of localStorage. Kill
// it at any point and run the same command again: it resumes from the parts
// the server already has. scripts/upload_resume_test.sh relies on exactly that.
//
// Options
//   --api URL           api origin (default http://localhost:8080)
//   --token JWT         bearer token (or RIGFORGE_TOKEN); or use --issuer/--user
//   --issuer URL        dev issuer origin, to fetch a token for --user
//   --user NAME         subject for the dev issuer token (default "cli")
//   --state FILE        where to remember the upload id (default <file>.upload.json)
//   --concurrency N     parts in flight at once (default 4)
//   --character ID      character for the motion (default "default")
//   --name NAME         asset name (default: the file name)
//   --no-complete       upload every part but do not call complete
//
// Output is one JSON object per line, so scripts can follow progress.

import { existsSync, openAsBlob, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { basename, extname } from "node:path";
import { parseArgs } from "node:util";
import { createApi } from "../src/api/client";
import { fetchTransport } from "../src/upload/transport";
import {
  ResumableUpload,
  UploadStopped,
  type FileSource,
  type PartTransport,
  type PersistedUpload,
  type ResumeStore,
} from "../src/upload/uploader";

const CONTENT_TYPES: Record<string, string> = {
  ".mp4": "video/mp4",
  ".m4v": "video/mp4",
  ".mov": "video/quicktime",
  ".webm": "video/webm",
  ".mkv": "video/x-matroska",
  ".avi": "video/x-msvideo",
};

const started = Date.now();
function log(event: string, fields: Record<string, unknown> = {}): void {
  console.log(JSON.stringify({ t: Date.now() - started, event, ...fields }));
}

/** ResumeStore backed by one JSON file. */
function fileStore(path: string): ResumeStore {
  const read = (): Record<string, PersistedUpload> =>
    existsSync(path) ? (JSON.parse(readFileSync(path, "utf8")) as Record<string, PersistedUpload>) : {};
  return {
    load: (key) => read()[key] ?? null,
    save(key, value) {
      writeFileSync(path, JSON.stringify({ ...read(), [key]: value }, null, 2));
    },
    remove(key) {
      const rest = read();
      delete rest[key];
      if (Object.keys(rest).length === 0) rmSync(path, { force: true });
      else writeFileSync(path, JSON.stringify(rest, null, 2));
    },
  };
}

/** Log every PUT to the bucket: this is the record of what was actually sent. */
function loggingTransport(inner: PartTransport): PartTransport {
  return {
    async put(url, body, onProgress, signal) {
      const part = Number(new URL(url).searchParams.get("partNumber"));
      log("put_start", { part, bytes: body.size });
      const etag = await inner.put(url, body, onProgress, signal);
      log("put_done", { part, bytes: body.size, etag });
      return etag;
    },
  };
}

async function fetchToken(issuer: string, user: string): Promise<string> {
  const response = await fetch(`${issuer}/token`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ sub: user }),
  });
  if (!response.ok) throw new Error(`issuer returned ${response.status}`);
  return ((await response.json()) as { access_token: string }).access_token;
}

async function main(): Promise<number> {
  const { values, positionals } = parseArgs({
    allowPositionals: true,
    options: {
      api: { type: "string", default: "http://localhost:8080" },
      token: { type: "string" },
      issuer: { type: "string" },
      user: { type: "string", default: "cli" },
      state: { type: "string" },
      concurrency: { type: "string", default: "4" },
      character: { type: "string", default: "default" },
      name: { type: "string" },
      "no-complete": { type: "boolean", default: false },
    },
  });
  const path = positionals[0];
  if (!path) {
    console.error("usage: upload-cli.ts <file> --api URL (--token JWT | --issuer URL --user NAME) [options]");
    return 2;
  }

  const token = values.token ?? process.env.RIGFORGE_TOKEN ?? (values.issuer ? await fetchToken(values.issuer, values.user) : "");
  if (!token) {
    console.error("no token: pass --token, set RIGFORGE_TOKEN, or pass --issuer");
    return 2;
  }

  // openAsBlob gives a Blob backed by the file: slice() is lazy, so a part's
  // bytes are only read from disk while that part is being sent.
  const blob = await openAsBlob(path);
  const source: FileSource = {
    name: basename(path),
    size: blob.size,
    type: CONTENT_TYPES[extname(path).toLowerCase()] ?? "video/mp4",
    lastModified: Math.floor(statSync(path).mtimeMs),
    slice: (start, end) => blob.slice(start, end),
  };

  const upload = new ResumableUpload(
    source,
    {
      api: createApi({ baseUrl: values.api, getToken: () => token }),
      transport: loggingTransport(fetchTransport),
      store: fileStore(values.state ?? `${path}.upload.json`),
    },
    {
      concurrency: Number(values.concurrency),
      characterId: values.character,
      name: values.name,
      complete: !values["no-complete"],
    },
  );

  let announced = false;
  upload.subscribe((state) => {
    if (!announced && state.phase === "uploading") {
      announced = true;
      const already = state.parts.filter((p) => p.resumed).map((p) => p.number);
      log("plan", {
        uploadId: state.uploadId,
        fileSize: state.fileSize,
        partSize: state.partSize,
        partCount: state.parts.length,
        resumed: already.length > 0,
        alreadyOnServer: already,
      });
    }
  });

  try {
    const result = await upload.start();
    log("result", {
      uploadId: result.uploadId,
      sentParts: [...result.sentParts].sort((a, b) => a - b),
      skippedParts: result.skippedParts,
      assetId: result.completed?.assetId ?? null,
      jobId: result.completed?.jobId ?? null,
    });
    return 0;
  } catch (err) {
    log("failed", { reason: err instanceof UploadStopped ? err.reason : String(err), phase: upload.state.phase });
    return 1;
  }
}

process.exitCode = await main();
