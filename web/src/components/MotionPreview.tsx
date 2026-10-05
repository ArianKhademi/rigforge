import { lazy, Suspense, useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { Asset } from "../api/types";
import { PlaybackClock } from "../lib/clock";
import { formatSeconds } from "../lib/format";
import styles from "./MotionPreview.module.css";

// three.js is by far the largest dependency; loading the viewer lazily keeps
// it out of the bundle for the browse and upload pages.
const MotionViewer = lazy(() => import("./MotionViewer").then((m) => ({ default: m.MotionViewer })));

interface Loaded {
  bones: number;
  hasMesh: boolean;
  duration: number;
}

/**
 * The preview: the motion on the character next to the source video, driven
 * by one clock, with shared transport controls.
 */
export function MotionPreview({ asset }: { asset: Asset }) {
  const duration = asset.durationSeconds ?? 0;
  const clock = useMemo(() => new PlaybackClock(duration), [duration]);
  const [time, setTime] = useState(0);
  const [playing, setPlaying] = useState(false);
  const [loop, setLoop] = useState(true);
  const [showSkeleton, setShowSkeleton] = useState(false);
  const [loaded, setLoaded] = useState<Loaded | null>(null);
  const [videoFailed, setVideoFailed] = useState(false);
  const scrubbing = useRef(false);

  // Mirror the clock into React a few times a second for the scrubber and the
  // play button. The 3D scene reads the clock directly every frame.
  useEffect(() => {
    let frame = 0;
    let last = 0;
    const update = (now: number) => {
      if (now - last > 80) {
        last = now;
        if (!scrubbing.current) setTime(clock.now());
        setPlaying(clock.playing);
      }
      frame = requestAnimationFrame(update);
    };
    frame = requestAnimationFrame(update);
    return () => cancelAnimationFrame(frame);
  }, [clock]);

  const attachVideo = useCallback((video: HTMLVideoElement | null) => clock.attach(video), [clock]);

  // Start playing as soon as the motion is ready.
  const onLoaded = useCallback(
    (info: Loaded) => {
      setLoaded(info);
      clock.play();
    },
    [clock],
  );

  const toggle = () => (clock.playing ? clock.pause() : clock.play());

  return (
    <div className={styles.wrap}>
      <div className={styles.views}>
        <figure
          className={styles.view}
          data-testid="motion-view"
          data-loaded={loaded ? "true" : "false"}
          data-bones={loaded?.bones ?? 0}
          data-mesh={loaded?.hasMesh ? "true" : "false"}
        >
          <Suspense fallback={<div className={styles.loading}>Loading viewer…</div>}>
            {asset.urls.motion && (
              <MotionViewer url={asset.urls.motion} clock={clock} showSkeleton={showSkeleton} onLoaded={onLoaded} />
            )}
          </Suspense>
          {!loaded && <div className={styles.loading}>Loading motion…</div>}
          <figcaption>Motion · drag to orbit, scroll to zoom</figcaption>
        </figure>

        <figure className={styles.view}>
          {asset.urls.preview && !videoFailed ? (
            <video
              ref={attachVideo}
              src={asset.urls.preview}
              poster={asset.urls.poster}
              muted
              playsInline
              preload="auto"
              // Once the video's metadata is in, it takes over as the clock
              // from wherever the internal clock had got to.
              onLoadedMetadata={(event) => clock.attach(event.currentTarget)}
              onError={() => {
                setVideoFailed(true);
                clock.attach(null);
              }}
              data-testid="source-video"
            />
          ) : (
            <div className={styles.loading}>Source preview unavailable</div>
          )}
          <figcaption>Source video</figcaption>
        </figure>
      </div>

      <div className={styles.transport}>
        <button className="btn" onClick={toggle} aria-label={playing ? "Pause" : "Play"} data-testid="play-toggle">
          {playing ? "Pause" : "Play"}
        </button>
        <input
          className={styles.scrubber}
          type="range"
          min={0}
          max={duration}
          step={1 / 30}
          value={Math.min(time, duration)}
          aria-label="Position"
          onPointerDown={() => (scrubbing.current = true)}
          onPointerUp={() => (scrubbing.current = false)}
          onChange={(event) => {
            const next = Number(event.target.value);
            setTime(next);
            clock.seek(next);
          }}
        />
        <span className={`${styles.time} mono`} data-testid="playhead">
          {formatSeconds(time)} / {formatSeconds(duration)}
        </span>
        <label className={styles.toggle}>
          <input
            type="checkbox"
            checked={loop}
            onChange={(event) => {
              setLoop(event.target.checked);
              clock.setLoop(event.target.checked);
            }}
          />
          Loop
        </label>
        <label className={styles.toggle}>
          <input
            type="checkbox"
            checked={showSkeleton || loaded?.hasMesh === false}
            disabled={loaded?.hasMesh === false}
            onChange={(event) => setShowSkeleton(event.target.checked)}
          />
          Skeleton
        </label>
      </div>
    </div>
  );
}
