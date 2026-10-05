import { useCallback, useEffect, useRef, useState } from "react";
import { useApi } from "../api/context";
import type { CompletedUpload } from "../api/types";
import { localStorageStore } from "./persistence";
import { uploadedBytes, type UploadState } from "./state";
import { xhrTransport } from "./transport";
import { ResumableUpload, UploadStopped, type UploadOptions, type UploadResult } from "./uploader";

export interface UploadController {
  state: UploadState | null;
  /** Bytes per second over the last few seconds, or 0 when idle. */
  speed: number;
  begin: (file: File, options: UploadOptions) => void;
  pause: () => void;
  resume: () => void;
  cancel: () => void;
  reset: () => void;
}

/**
 * React binding for the upload engine: owns one ResumableUpload, mirrors its
 * state into React, and reports the finished upload through onDone.
 */
export function useUpload(onDone: (result: CompletedUpload) => void): UploadController {
  const api = useApi();
  const [state, setState] = useState<UploadState | null>(null);
  const [speed, setSpeed] = useState(0);
  const upload = useRef<ResumableUpload | null>(null);
  const done = useRef(onDone);
  done.current = onDone;

  const run = useCallback((promise: Promise<UploadResult>) => {
    promise.then(
      (result) => {
        if (result.completed) done.current(result.completed);
      },
      (err: unknown) => {
        // Pausing, cancelling and running out of retries are states the UI
        // already shows; anything else is in state.error. Either way the
        // rejection is handled here so it never becomes an unhandled one.
        if (!(err instanceof UploadStopped)) console.warn("upload failed:", err);
      },
    );
  }, []);

  const begin = useCallback(
    (file: File, options: UploadOptions) => {
      const next = new ResumableUpload(file, { api, transport: xhrTransport, store: localStorageStore() }, options);
      upload.current = next;
      setState(next.state);
      next.subscribe(setState);
      run(next.start());
    },
    [api, run],
  );

  const pause = useCallback(() => upload.current?.pause(), []);
  const resume = useCallback(() => {
    if (upload.current) run(upload.current.resume());
  }, [run]);
  const cancel = useCallback(() => void upload.current?.cancel(), []);
  const reset = useCallback(() => {
    upload.current = null;
    setState(null);
  }, []);

  // Transfer speed: sample the byte counter once a second and average the
  // last five samples, so the number is steady enough to read.
  const phase = state?.phase;
  useEffect(() => {
    if (phase !== "uploading") {
      setSpeed(0);
      return;
    }
    const samples: number[] = [];
    let last = upload.current ? uploadedBytes(upload.current.state) : 0;
    const timer = setInterval(() => {
      const now = upload.current ? uploadedBytes(upload.current.state) : last;
      samples.push(Math.max(0, now - last));
      last = now;
      if (samples.length > 5) samples.shift();
      setSpeed(samples.reduce((a, b) => a + b, 0) / samples.length);
    }, 1000);
    return () => clearInterval(timer);
  }, [phase]);

  // If the connection came back after an interruption, carry on by itself.
  useEffect(() => {
    if (phase !== "interrupted") return;
    const onOnline = () => resume();
    window.addEventListener("online", onOnline);
    return () => window.removeEventListener("online", onOnline);
  }, [phase, resume]);

  // Leaving the page pauses the upload: the parts already sent stay on the
  // server and picking the same file again resumes.
  useEffect(() => () => upload.current?.pause(), []);

  return { state, speed, begin, pause, resume, cancel, reset };
}
