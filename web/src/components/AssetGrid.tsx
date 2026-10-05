import { Link } from "react-router-dom";
import type { Asset } from "../api/types";
import { formatBytes, formatDuration, formatRelative } from "../lib/format";
import styles from "./AssetGrid.module.css";
import { retryText, StatusBadge } from "./JobStatus";

export function AssetGrid({ assets }: { assets: Asset[] }) {
  return (
    <ul className={styles.grid} data-testid="asset-grid">
      {assets.map((asset) => (
        <li key={asset.id}>
          <AssetCard asset={asset} />
        </li>
      ))}
    </ul>
  );
}

export function AssetCard({ asset }: { asset: Asset }) {
  const job = asset.job;
  const processing = asset.status === "processing";
  const retry = job ? retryText(job) : null;
  return (
    <Link to={`/assets/${asset.id}`} className={styles.card} data-testid="asset-card" aria-label={asset.name}>
      <div className={styles.poster}>
        {asset.urls.poster ? (
          <img src={asset.urls.poster} alt="" loading="lazy" />
        ) : (
          <div className={styles.placeholder} aria-hidden="true">
            {asset.status === "failed" ? "!" : "…"}
          </div>
        )}
        {asset.durationSeconds != null && (
          <span className={`${styles.duration} mono`}>{formatDuration(asset.durationSeconds)}</span>
        )}
        {processing && job && job.status !== "queued" && (
          <div className={styles.bar} aria-hidden="true">
            <div style={{ width: `${job.progress}%` }} />
          </div>
        )}
      </div>
      <div className={styles.body}>
        <div className={styles.titleRow}>
          <h3 className={styles.name} title={asset.name}>
            {asset.name}
          </h3>
          <StatusBadge asset={asset} />
        </div>
        <p className={styles.meta}>
          {formatBytes(asset.sourceSize)} · {formatRelative(asset.createdAt)}
        </p>
        {/* While a retry is pending or running, say so and show what went wrong. */}
        {processing && retry && (
          <p className={styles.retry} data-testid="retry-count">
            {retry}
            {job?.error ? `: ${job.error}` : ""}
          </p>
        )}
        {asset.status === "failed" && job && (
          <p className={styles.error} data-testid="job-error">
            <span>{job.error ?? "Processing failed."}</span>
            <span className={styles.attempts}>
              Gave up after attempt {job.attempt} of {job.maxAttempts}
            </span>
          </p>
        )}
      </div>
    </Link>
  );
}
