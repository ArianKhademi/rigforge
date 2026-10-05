import { useQueryClient } from "@tanstack/react-query";
import { useCallback, useMemo, useRef, useState, type DragEvent } from "react";
import { useNavigate } from "react-router-dom";
import { useApi } from "../api/context";
import { CharacterPicker } from "../components/CharacterPicker";
import { UploadProgress } from "../components/UploadProgress";
import { formatBytes, formatRelative } from "../lib/format";
import { forgetUpload, listUnfinished, localStorageStore } from "../upload/persistence";
import { fingerprint, type PersistedUpload } from "../upload/uploader";
import { useUpload } from "../upload/useUpload";
import styles from "./UploadPage.module.css";

const PART_SIZE = 64 * 1024 * 1024; // only for the estimate shown before the server answers

export function UploadPage() {
  const api = useApi();
  const navigate = useNavigate();
  const queryClient = useQueryClient();

  const [file, setFile] = useState<File | null>(null);
  const [name, setName] = useState("");
  const [characterId, setCharacterId] = useState("default");
  const [dragging, setDragging] = useState(false);
  const [rejected, setRejected] = useState<string | null>(null);
  const [unfinished, setUnfinished] = useState<PersistedUpload[]>(() => listUnfinished());
  const picker = useRef<HTMLInputElement>(null);

  const upload = useUpload((result) => {
    void queryClient.invalidateQueries({ queryKey: ["assets"] });
    navigate(`/assets/${result.assetId}`);
  });
  const state = upload.state;
  const busy = state != null && state.phase !== "cancelled" && state.phase !== "error";

  // If this exact file has an unfinished upload on this device, say so up front.
  const saved = useMemo(() => (file ? localStorageStore().load(fingerprint(file)) : null), [file]);

  const choose = useCallback((candidate: File | undefined) => {
    if (!candidate) return;
    // Some systems report no MIME type for less common containers (.mkv,
    // .mov), so fall back to the extension. The worker's ffprobe is the real
    // check; this only catches obvious mistakes early.
    const looksLikeVideo = candidate.type.startsWith("video/") || /\.(mp4|m4v|mov|mkv|webm|avi)$/i.test(candidate.name);
    if (!looksLikeVideo) {
      setRejected(`“${candidate.name}” is not a video file.`);
      return;
    }
    setRejected(null);
    setFile(candidate);
    setName(candidate.name.replace(/\.[^.]+$/, ""));
  }, []);

  const onDrop = (event: DragEvent) => {
    event.preventDefault();
    setDragging(false);
    choose(event.dataTransfer.files[0]);
  };

  const start = () => {
    if (!file) return;
    upload.begin(file, { name: name.trim() || undefined, characterId });
  };

  const startOver = () => {
    upload.reset();
    setFile(null);
    setUnfinished(listUnfinished());
  };

  const discard = async (item: PersistedUpload) => {
    // Abort on the server so the bucket releases the parts, then forget it here.
    await api.abortUpload(item.uploadId).catch(() => undefined);
    forgetUpload(item.uploadId);
    setUnfinished(listUnfinished());
  };

  return (
    <div className={styles.page}>
      <div>
        <h1>Upload a video</h1>
        <p className="muted">
          One person, visible head to feet, filmed with a still camera works best. Large files are sent in 64 MiB parts
          and resume where they left off.
        </p>
      </div>

      {!busy && (
        <div
          className={`${styles.drop} ${dragging ? styles.dragging : ""} ${file ? styles.hasFile : ""}`}
          onDragOver={(event) => {
            event.preventDefault();
            setDragging(true);
          }}
          onDragLeave={() => setDragging(false)}
          onDrop={onDrop}
          data-testid="dropzone"
        >
          <input
            ref={picker}
            type="file"
            accept="video/*"
            hidden
            data-testid="video-file"
            onChange={(event) => {
              choose(event.target.files?.[0]);
              event.target.value = "";
            }}
          />
          {file ? (
            <>
              <p className={styles.fileName}>{file.name}</p>
              <p className="muted mono">
                {formatBytes(file.size)} · {Math.ceil(file.size / PART_SIZE)}{" "}
                {Math.ceil(file.size / PART_SIZE) === 1 ? "part" : "parts"}
              </p>
              <button type="button" className="btn btn-ghost" onClick={() => picker.current?.click()}>
                Choose a different file
              </button>
            </>
          ) : (
            <>
              <p className={styles.dropTitle}>Drop a video here</p>
              <p className="muted">or</p>
              <button type="button" className="btn" onClick={() => picker.current?.click()}>
                Browse files
              </button>
            </>
          )}
        </div>
      )}
      {rejected && (
        <p className="notice notice-error" role="alert">
          {rejected}
        </p>
      )}

      {file && !busy && (
        <div className={`panel ${styles.form}`}>
          {saved && (
            <p className="notice notice-info" data-testid="resume-hint">
              An upload of this file was interrupted with {saved.completedParts.length} of {saved.partCount} parts sent.
              It will resume from there.
            </p>
          )}
          <label className="field">
            <span>Asset name</span>
            <input className="input" value={name} maxLength={120} onChange={(e) => setName(e.target.value)} />
          </label>
          <CharacterPicker value={characterId} onChange={setCharacterId} />
          <div>
            <button className="btn btn-primary" onClick={start} data-testid="create-motion">
              {saved ? "Resume upload and create motion" : "Create motion"}
            </button>
          </div>
        </div>
      )}

      {state && (
        <div className="panel">
          <UploadProgress
            state={state}
            speed={upload.speed}
            onPause={upload.pause}
            onResume={upload.resume}
            onCancel={upload.cancel}
          />
          {(state.phase === "cancelled" || state.phase === "error") && (
            <button className="btn" style={{ marginTop: "0.85rem" }} onClick={startOver}>
              Start over
            </button>
          )}
        </div>
      )}

      {!busy && unfinished.length > 0 && (
        <section className={styles.unfinished} aria-label="Unfinished uploads">
          <h2>Unfinished uploads on this device</h2>
          <ul>
            {unfinished.map((item) => (
              <li key={item.uploadId} className="panel">
                <div>
                  <strong>{item.fileName}</strong>
                  <p className="muted">
                    {item.completedParts.length} of {item.partCount} parts sent · {formatBytes(item.fileSize)} ·{" "}
                    {formatRelative(new Date(item.updatedAt).toISOString())}
                  </p>
                  <p className="muted">Choose the same file again to resume.</p>
                </div>
                <button className="btn btn-danger" onClick={() => void discard(item)}>
                  Discard
                </button>
              </li>
            ))}
          </ul>
        </section>
      )}
    </div>
  );
}
