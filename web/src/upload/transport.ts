import type { PartTransport } from "./uploader";

/** A PUT to the bucket that the bucket itself rejected (as opposed to a network failure). */
export class PartRejected extends Error {
  constructor(readonly status: number) {
    super(`the bucket rejected the part (HTTP ${status})`);
    this.name = "PartRejected";
  }
}

function requireETag(etag: string | null): string {
  if (!etag) {
    // The request succeeded but the browser hides the header: the bucket's
    // CORS policy must list ETag under ExposeHeaders (see docs/r2-cors.json).
    throw new Error("the bucket did not expose an ETag header; check its CORS configuration");
  }
  return etag;
}

/**
 * Browser transport. XMLHttpRequest rather than fetch because it is still the
 * only API that reports upload progress, which the per-part progress bars need.
 */
export const xhrTransport: PartTransport = {
  put(url, body, onProgress, signal) {
    return new Promise<string>((resolve, reject) => {
      // An "abort" listener only sees future aborts. A signal that is already
      // aborted (the upload was stopped while this part waited for its URL)
      // must not start a request at all, or a whole part would be sent and
      // then thrown away.
      if (signal.aborted) return reject(signal.reason);
      const xhr = new XMLHttpRequest();
      xhr.open("PUT", url);
      xhr.upload.onprogress = (event) => onProgress(event.loaded);
      xhr.onload = () => {
        if (xhr.status >= 200 && xhr.status < 300) {
          try {
            resolve(requireETag(xhr.getResponseHeader("ETag")));
          } catch (err) {
            reject(err);
          }
        } else {
          reject(new PartRejected(xhr.status));
        }
      };
      xhr.onerror = () => reject(new Error("network error while uploading a part"));
      xhr.onabort = () => reject(signal.reason ?? new Error("upload aborted"));
      // Pausing or cancelling aborts the request; S3 discards a partial part.
      signal.addEventListener("abort", () => xhr.abort(), { once: true });
      xhr.send(body);
    });
  },
};

/** fetch transport for Node (the upload CLI). No progress events: a part is
 *  reported as fully sent when its request completes. */
export const fetchTransport: PartTransport = {
  async put(url, body, onProgress, signal) {
    const response = await fetch(url, { method: "PUT", body, signal });
    if (!response.ok) throw new PartRejected(response.status);
    onProgress(body.size);
    return requireETag(response.headers.get("ETag"));
  },
};
