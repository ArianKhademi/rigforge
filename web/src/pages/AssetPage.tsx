import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { Link, NavLink, useNavigate, useParams } from "react-router-dom";
import { ApiError } from "../api/client";
import { useApi } from "../api/context";
import type { Asset } from "../api/types";
import { CharacterPicker } from "../components/CharacterPicker";
import { JobProgress, StatusBadge } from "../components/JobStatus";
import { MotionPreview } from "../components/MotionPreview";
import { useJobStream } from "../hooks/useJobStream";
import { formatBytes, formatDuration, formatRelative } from "../lib/format";
import styles from "./AssetPage.module.css";

export function AssetPage({ tab }: { tab: "preview" | "export" }) {
  const { id = "" } = useParams();
  const api = useApi();

  const { data: asset, error, isPending } = useQuery({
    queryKey: ["asset", id],
    queryFn: ({ signal }) => api.getAsset(id, signal),
    // A deleted or foreign asset is a 404; retrying will not change that.
    retry: (count, err) => !(err instanceof ApiError && err.status === 404) && count < 2,
  });
  // Live progress while the job runs (server-sent events).
  useJobStream(id, asset?.job);

  if (error) {
    return (
      <div className={styles.page}>
        <p className="notice notice-error" role="alert">
          {error instanceof ApiError && error.status === 404 ? "This asset does not exist." : error.message}
        </p>
        <Link to="/" className="btn">
          Back to assets
        </Link>
      </div>
    );
  }
  if (isPending) return <p className="muted">Loading…</p>;

  const tabClass = ({ isActive }: { isActive: boolean }) => (isActive ? `${styles.tab} ${styles.activeTab}` : styles.tab);
  return (
    <div className={styles.page}>
      <Link to="/" className={styles.back}>
        ← Assets
      </Link>
      <div className={styles.header}>
        <div className={styles.titleBlock}>
          <h1>{asset.name}</h1>
          <p className="muted">
            {asset.durationSeconds != null && <>{formatDuration(asset.durationSeconds)} · </>}
            {asset.frameCount != null && <>{asset.frameCount} frames · </>}
            {formatBytes(asset.sourceSize)} source · uploaded {formatRelative(asset.createdAt)}
          </p>
        </div>
        <StatusBadge asset={asset} />
      </div>

      {asset.status === "processing" && asset.job && (
        <div className="panel">
          <JobProgress job={asset.job} />
        </div>
      )}

      {asset.status === "failed" && <FailedPanel asset={asset} />}

      {asset.status === "ready" && (
        <>
          <nav className={styles.tabs} aria-label="Asset views">
            <NavLink to={`/assets/${asset.id}`} end className={tabClass}>
              Preview
            </NavLink>
            <NavLink to={`/assets/${asset.id}/export`} className={tabClass}>
              Export
            </NavLink>
          </nav>
          {tab === "preview" ? <MotionPreview asset={asset} /> : <ExportPanel asset={asset} />}
        </>
      )}

      {asset.status !== "ready" && <DeleteAsset asset={asset} />}
    </div>
  );
}

function useReprocess(asset: Asset) {
  const api = useApi();
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (characterId: string) => api.reprocessAsset(asset.id, characterId),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["asset", asset.id] });
      void queryClient.invalidateQueries({ queryKey: ["assets"] });
    },
  });
}

function FailedPanel({ asset }: { asset: Asset }) {
  const [characterId, setCharacterId] = useState(asset.job?.characterId ?? "default");
  const reprocess = useReprocess(asset);
  const job = asset.job;
  return (
    <div className={`panel ${styles.stack}`}>
      <div className="notice notice-error" role="alert" data-testid="failure">
        <strong>Processing failed.</strong> {job?.error ?? "No error was recorded."}
        {job && (
          <p className={styles.attempts}>
            Gave up after attempt {job.attempt} of {job.maxAttempts}. The job is in the dead-letter queue.
          </p>
        )}
      </div>
      <CharacterPicker value={characterId} onChange={setCharacterId} disabled={reprocess.isPending} />
      {reprocess.error && <p className="notice notice-error">{reprocess.error.message}</p>}
      <div>
        <button className="btn btn-primary" disabled={reprocess.isPending} onClick={() => reprocess.mutate(characterId)}>
          {reprocess.isPending ? "Queuing…" : "Try again"}
        </button>
      </div>
    </div>
  );
}

function ExportPanel({ asset }: { asset: Asset }) {
  const api = useApi();
  const [characterId, setCharacterId] = useState(asset.job?.characterId ?? "default");
  const reprocess = useReprocess(asset);

  // Ask the api for a short-lived presigned URL and hand it to the browser.
  // The URL carries a Content-Disposition, so navigating to it downloads the
  // file instead of opening it; the bytes come straight from the bucket.
  const download = useMutation({
    mutationFn: (format: "glb" | "bvh") => api.exportAsset(asset.id, format),
    onSuccess: (link) => {
      const anchor = document.createElement("a");
      anchor.href = link.url;
      anchor.download = link.filename;
      document.body.append(anchor);
      anchor.click();
      anchor.remove();
    },
  });

  return (
    <div className={styles.exportGrid}>
      <section className={`panel ${styles.stack}`}>
        <h2>Download</h2>
        <ul className={styles.files}>
          <li>
            <div>
              <strong>motion.glb</strong>
              <p className="muted">
                glTF 2.0 binary: skeleton, skin, one animation, and the character mesh when one was selected. Opens in
                Blender, three.js, Unity and Unreal importers.
              </p>
            </div>
            <button className="btn btn-primary" disabled={download.isPending} onClick={() => download.mutate("glb")} data-testid="download-glb">
              Download GLB
            </button>
          </li>
          <li>
            <div>
              <strong>motion.bvh</strong>
              <p className="muted">
                The same animation as BVH (centimetres, ZXY rotations) for motion-capture tools.
              </p>
            </div>
            <button className="btn" disabled={download.isPending} onClick={() => download.mutate("bvh")} data-testid="download-bvh">
              Download BVH
            </button>
          </li>
        </ul>
        <p className="muted" style={{ fontSize: "0.8rem" }}>
          Download links are presigned and expire after two minutes.
        </p>
        {download.error && <p className="notice notice-error">{download.error.message}</p>}
      </section>

      <section className={`panel ${styles.stack}`}>
        <h2>Apply to another character</h2>
        <p className="muted">Runs the job again for this video and replaces the files above.</p>
        <CharacterPicker value={characterId} onChange={setCharacterId} disabled={reprocess.isPending} />
        {reprocess.error && <p className="notice notice-error">{reprocess.error.message}</p>}
        <div>
          <button className="btn" disabled={reprocess.isPending} onClick={() => reprocess.mutate(characterId)}>
            {reprocess.isPending ? "Queuing…" : "Re-run job"}
          </button>
        </div>
      </section>

      <DeleteAsset asset={asset} />
    </div>
  );
}

function DeleteAsset({ asset }: { asset: Asset }) {
  const api = useApi();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [confirming, setConfirming] = useState(false);
  const remove = useMutation({
    mutationFn: () => api.deleteAsset(asset.id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["assets"] });
      navigate("/");
    },
  });
  return (
    <section className={`panel ${styles.danger}`}>
      <div>
        <h2>Delete asset</h2>
        <p className="muted">Removes the source video and every output from storage. This cannot be undone.</p>
        {remove.error && <p className="notice notice-error">{remove.error.message}</p>}
      </div>
      {confirming ? (
        <div className={styles.confirm}>
          <button className="btn btn-danger" disabled={remove.isPending} onClick={() => remove.mutate()} data-testid="confirm-delete">
            {remove.isPending ? "Deleting…" : "Yes, delete it"}
          </button>
          <button className="btn btn-ghost" onClick={() => setConfirming(false)}>
            Keep
          </button>
        </div>
      ) : (
        <button className="btn btn-danger" onClick={() => setConfirming(true)} data-testid="delete-asset">
          Delete
        </button>
      )}
    </section>
  );
}
