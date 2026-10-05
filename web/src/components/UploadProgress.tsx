import { formatBytes, formatDuration } from "../lib/format";
import { countParts, progress, uploadedBytes, type PartState, type UploadState } from "../upload/state";
import styles from "./UploadProgress.module.css";

const PHASE_TEXT: Record<UploadState["phase"], string> = {
  idle: "Starting",
  preparing: "Checking what the server already has",
  uploading: "Uploading",
  paused: "Paused",
  interrupted: "Connection lost",
  completing: "Finishing",
  done: "Uploaded",
  cancelled: "Cancelled",
  error: "Upload failed",
};

interface Props {
  state: UploadState;
  speed: number;
  onPause: () => void;
  onResume: () => void;
  onCancel: () => void;
}

export function UploadProgress({ state, speed, onPause, onResume, onCancel }: Props) {
  const fraction = progress(state);
  const sent = uploadedBytes(state);
  const done = countParts(state, "done");
  const resumed = state.parts.filter((p) => p.resumed).length;
  const remainingSeconds = speed > 0 ? (state.fileSize - sent) / speed : null;
  const active = state.phase === "uploading" || state.phase === "preparing" || state.phase === "completing";

  return (
    <section className={styles.wrap} aria-label="Upload progress" data-testid="upload-progress" data-phase={state.phase}>
      <div className={styles.head}>
        <div>
          <strong>{PHASE_TEXT[state.phase]}</strong>
          <span className={styles.file}>{state.fileName}</span>
        </div>
        <span className={`${styles.percent} mono`}>{Math.floor(fraction * 100)}%</span>
      </div>

      <div className={styles.track} role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.floor(fraction * 100)}>
        <div className={styles.fill} style={{ width: `${fraction * 100}%` }} />
      </div>

      <p className={`${styles.stats} mono`}>
        <span>
          {formatBytes(sent)} of {formatBytes(state.fileSize)}
        </span>
        <span data-testid="parts-done">
          {done} of {state.parts.length} parts
        </span>
        {state.phase === "uploading" && (
          <span>
            {formatBytes(speed)}/s{remainingSeconds != null && ` · ${formatDuration(remainingSeconds)} left`}
          </span>
        )}
      </p>

      {resumed > 0 && (
        <p className="notice notice-info" data-testid="resumed-note">
          Resumed: {resumed} {resumed === 1 ? "part was" : "parts were"} already on the server and{" "}
          {resumed === 1 ? "is" : "are"} not being sent again.
        </p>
      )}
      {state.phase === "interrupted" && (
        <p className="notice notice-warn" role="alert">
          The connection dropped ({state.error}). {done} of {state.parts.length} parts are safely uploaded; resuming
          sends only the rest.
        </p>
      )}
      {state.phase === "error" && (
        <p className="notice notice-error" role="alert">
          {state.error}
        </p>
      )}

      {state.parts.length > 0 && <PartGrid parts={state.parts} />}

      <div className={styles.actions}>
        {state.phase === "uploading" && (
          <button className="btn" onClick={onPause}>
            Pause
          </button>
        )}
        {(state.phase === "paused" || state.phase === "interrupted") && (
          <button className="btn btn-primary" onClick={onResume}>
            Resume
          </button>
        )}
        {(active || state.phase === "paused" || state.phase === "interrupted") && state.phase !== "completing" && (
          <button className="btn btn-danger" onClick={onCancel}>
            Cancel upload
          </button>
        )}
      </div>
    </section>
  );
}

/** One cell per part. An uploading cell fills from the bottom as bytes go out. */
export function PartGrid({ parts }: { parts: PartState[] }) {
  return (
    <div>
      <ol className={styles.parts} aria-label="Parts" data-testid="part-grid">
        {parts.map((part) => (
          <li
            key={part.number}
            className={`${styles.part} ${styles[part.status]} ${part.resumed ? styles.resumed : ""}`}
            data-status={part.status}
            title={`Part ${part.number}: ${part.resumed ? "already on the server" : part.status} (${formatBytes(part.size)})`}
          >
            {part.status === "uploading" && (
              <span className={styles.partFill} style={{ height: `${(part.loaded / part.size) * 100}%` }} />
            )}
            <span className={styles.partNumber}>{part.number}</span>
          </li>
        ))}
      </ol>
      <ul className={styles.legend} aria-hidden="true">
        <li className={styles.done}>uploaded</li>
        <li className={`${styles.done} ${styles.resumed}`}>resumed</li>
        <li className={styles.uploading}>in flight</li>
        <li className={styles.pending}>waiting</li>
        <li className={styles.failed}>failed</li>
      </ul>
    </div>
  );
}
