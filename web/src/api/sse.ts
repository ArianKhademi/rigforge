import { ApiError } from "./client";
import type { Job } from "./types";

/**
 * Follow a job over server-sent events until the server ends the stream
 * (which it does when the job is done or failed).
 *
 * This uses fetch and reads the body as a stream instead of the browser's
 * EventSource, because EventSource cannot send an Authorization header and
 * putting the token in the URL would leak it into logs and history.
 */
export async function streamJob(
  options: { baseUrl: string; getToken: () => string | null },
  jobId: string,
  onJob: (job: Job) => void,
  signal: AbortSignal,
): Promise<void> {
  const response = await fetch(`${options.baseUrl}/api/jobs/${jobId}/events`, {
    headers: { Authorization: `Bearer ${options.getToken() ?? ""}`, Accept: "text/event-stream" },
    signal,
  });
  if (!response.ok || !response.body) {
    throw new ApiError(response.status, "stream_failed", `job stream returned ${response.status}`);
  }

  const reader = response.body.pipeThrough(new TextDecoderStream()).getReader();
  let buffer = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) return;
    buffer += value;
    // Events are separated by a blank line. A chunk can end mid-event, so
    // only complete events are taken out of the buffer.
    for (let end = buffer.indexOf("\n\n"); end >= 0; end = buffer.indexOf("\n\n")) {
      const event = buffer.slice(0, end);
      buffer = buffer.slice(end + 2);
      const data = parseEvent(event);
      if (data !== null) onJob(JSON.parse(data) as Job);
    }
  }
}

/** The data payload of one SSE event, or null for comments and keepalives. */
export function parseEvent(event: string): string | null {
  const data = event
    .split("\n")
    .filter((line) => line.startsWith("data:"))
    .map((line) => line.slice(5).trimStart());
  return data.length > 0 ? data.join("\n") : null;
}
