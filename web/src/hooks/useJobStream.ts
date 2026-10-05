import { useQueryClient } from "@tanstack/react-query";
import { useEffect } from "react";
import { useApi } from "../api/context";
import { streamJob } from "../api/sse";
import { isTerminal, type Asset, type Job } from "../api/types";

/**
 * While a job is running, keep the cached asset in sync with the job's live
 * event stream. When the job reaches a terminal state the asset is refetched,
 * because that is when its status and output URLs change.
 */
export function useJobStream(assetId: string, job: Job | null | undefined): void {
  const api = useApi();
  const queryClient = useQueryClient();
  const jobId = job?.id;
  const active = job != null && !isTerminal(job.status);

  useEffect(() => {
    if (!jobId || !active) return;
    const controller = new AbortController();
    let finished = false;

    const onJob = (next: Job) => {
      queryClient.setQueryData<Asset>(["asset", assetId], (asset) => (asset ? { ...asset, job: next } : asset));
      if (isTerminal(next.status)) {
        finished = true;
        void queryClient.invalidateQueries({ queryKey: ["asset", assetId] });
        void queryClient.invalidateQueries({ queryKey: ["assets"] });
      }
    };

    // The stream can drop (api restart, proxy timeout). Reconnect with a
    // short, growing delay; the first event after reconnecting is always the
    // current state, so nothing is missed.
    void (async () => {
      for (let attempt = 0; !controller.signal.aborted && !finished; attempt++) {
        try {
          await streamJob(api, jobId, onJob, controller.signal);
          attempt = 0;
        } catch {
          // fall through to the retry delay
        }
        if (controller.signal.aborted || finished) return;
        await new Promise((resolve) => setTimeout(resolve, Math.min(5000, 500 * 2 ** attempt)));
        // A fallback in case the stream cannot be established at all.
        void queryClient.invalidateQueries({ queryKey: ["asset", assetId] });
      }
    })();

    return () => controller.abort();
  }, [api, assetId, jobId, active, queryClient]);
}
