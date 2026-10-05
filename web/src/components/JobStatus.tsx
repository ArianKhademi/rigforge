import type { Asset, Job, JobStatus as Status } from "../api/types";
import { formatClock } from "../lib/format";
import styles from "./JobStatus.module.css";

// The pipeline stages in order; the job's status is the stage it is in.
export const STAGES: { status: Status; label: string }[] = [
  { status: "queued", label: "Queued" },
  { status: "transcoding", label: "Transcoding" },
  { status: "extracting", label: "Extracting pose" },
  { status: "writing", label: "Writing motion" },
  { status: "uploading", label: "Storing outputs" },
  { status: "done", label: "Done" },
];

export function stageLabel(status: Status): string {
  if (status === "failed") return "Failed";
  return STAGES.find((s) => s.status === status)?.label ?? status;
}

/** "Retry 1 of 2" once a job is past its first attempt, otherwise null. */
export function retryText(job: Job): string | null {
  if (job.attempt <= 1) return null;
  return `Retry ${job.attempt - 1} of ${job.maxAttempts - 1}`;
}

type Tone = "ok" | "busy" | "wait" | "err";

function tone(asset: Asset): Tone {
  if (asset.status === "ready") return "ok";
  if (asset.status === "failed") return "err";
  return asset.job?.status === "queued" ? "wait" : "busy";
}

/** Compact status pill for the browse grid and the asset header. */
export function StatusBadge({ asset }: { asset: Asset }) {
  const job = asset.job;
  let text = "Processing";
  if (asset.status === "ready") text = "Ready";
  else if (asset.status === "failed") text = "Failed";
  else if (job) {
    const waitingForRetry = job.status === "queued" && job.retryAt != null;
    text = waitingForRetry ? "Waiting to retry" : stageLabel(job.status);
    if (job.status !== "queued") text += ` ${job.progress}%`;
  }
  return (
    <span className={`${styles.badge} ${styles[tone(asset)]}`} data-testid="status-badge">
      <span className={styles.dot} aria-hidden="true" />
      {text}
    </span>
  );
}

/** Full progress view for the asset page: stage list, bar, attempts, errors. */
export function JobProgress({ job }: { job: Job }) {
  const current = STAGES.findIndex((s) => s.status === job.status);
  const retry = retryText(job);
  return (
    <div className={styles.progress} data-testid="job-progress">
      <div className={styles.head}>
        <strong>{job.status === "queued" && job.retryAt ? "Waiting to retry" : stageLabel(job.status)}</strong>
        <span className="mono muted">
          {job.progress}% · attempt {job.attempt} of {job.maxAttempts}
        </span>
      </div>
      <div className={styles.track} role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={job.progress}>
        <div className={styles.fill} style={{ width: `${job.progress}%` }} />
      </div>
      <ol className={styles.stages}>
        {STAGES.slice(0, -1).map((stage, i) => (
          <li key={stage.status} className={i < current ? styles.past : i === current ? styles.now : undefined}>
            {stage.label}
          </li>
        ))}
      </ol>
      {retry && job.error && (
        <p className="notice notice-warn">
          <strong>{retry}.</strong> The previous attempt failed: {job.error}
          {job.retryAt && job.status === "queued" && <> Next attempt at {formatClock(job.retryAt)}.</>}
        </p>
      )}
    </div>
  );
}
