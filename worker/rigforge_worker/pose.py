"""Pose extraction with MediaPipe Pose Landmarker, plus the clean-up that turns
raw per-frame detections into a continuous track: gap filling and smoothing.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass, field
from pathlib import Path

import numpy as np

from . import media
from .errors import PermanentError

NUM_LANDMARKS = 33

# Runs of missing frames shorter than this are bridged by interpolation
# (a third of a second at 30 fps: a brief occlusion or a missed detection).
# Longer runs are recorded as gaps; the pose is held rather than invented.
MAX_INTERPOLATED_GAP = 10

# A person must be found in at least this many frames (half a second) and in
# at least this share of the video for the result to be worth producing.
MIN_DETECTED_FRAMES = 15
MIN_DETECTED_FRACTION = 0.2


@dataclass
class PoseTrack:
    """Per-frame landmarks for one video.

    world       (T, 33, 3) metres, origin at the hip centre, MediaPipe axes
                (x right, y DOWN, z away from the camera). NaN where undetected.
    image       (T, 33, 2) pixel coordinates in the analysed frame.
    visibility  (T, 33) MediaPipe's confidence that a landmark is visible, 0..1.
    detected    (T,) whether a person was found in that frame.
    gaps        [start, end) frame ranges where the subject was lost for
                MAX_INTERPOLATED_GAP frames or more (filled by fill_gaps).
    """

    world: np.ndarray
    image: np.ndarray
    visibility: np.ndarray
    detected: np.ndarray
    width: int
    height: int
    fps: float
    gaps: list[tuple[int, int]] = field(default_factory=list)

    @property
    def frame_count(self) -> int:
        return len(self.detected)


def extract(video: Path, model_path: Path, on_progress: Callable[[float], None] | None = None) -> PoseTrack:
    """Run the pose landmarker over every frame of the working copy."""
    # Imported here so the queue and maths modules (and their tests) do not
    # need MediaPipe's native libraries just to be imported.
    import mediapipe as mp
    from mediapipe.tasks import python as mp_tasks
    from mediapipe.tasks.python import vision

    if not model_path.exists():
        raise RuntimeError(f"pose model not found at {model_path}; run `make models`")

    info = media.probe(video)
    options = vision.PoseLandmarkerOptions(
        # CPU on purpose: it behaves the same on a laptop and in the Linux
        # worker image (which has no GPU), and the GPU delegate cannot start in
        # headless environments.
        base_options=mp_tasks.BaseOptions(model_asset_path=str(model_path), delegate=mp_tasks.BaseOptions.Delegate.CPU),
        # VIDEO mode tracks the person from frame to frame instead of running
        # the (slower, jumpier) person detector on every frame.
        running_mode=vision.RunningMode.VIDEO,
        num_poses=1,
    )

    world, image, visibility, detected = [], [], [], []
    with vision.PoseLandmarker.create_from_options(options) as landmarker:
        for index, frame in enumerate(media.read_frames(video, info.width, info.height)):
            mp_image = mp.Image(image_format=mp.ImageFormat.SRGB, data=np.ascontiguousarray(frame))
            # VIDEO mode needs strictly increasing timestamps in milliseconds.
            result = landmarker.detect_for_video(mp_image, int(round(index * 1000 / media.FPS)))
            if result.pose_world_landmarks:
                w, im = result.pose_world_landmarks[0], result.pose_landmarks[0]
                world.append([(p.x, p.y, p.z) for p in w])
                # Image landmarks are normalised to 0..1; store pixels.
                image.append([(p.x * info.width, p.y * info.height) for p in im])
                visibility.append([p.visibility for p in w])
                detected.append(True)
            else:
                world.append(np.full((NUM_LANDMARKS, 3), np.nan))
                image.append(np.full((NUM_LANDMARKS, 2), np.nan))
                visibility.append(np.zeros(NUM_LANDMARKS))
                detected.append(False)
            if on_progress and info.frame_count:
                on_progress(min(1.0, (index + 1) / info.frame_count))

    require_person(np.asarray(detected, dtype=bool))
    return PoseTrack(
        world=np.asarray(world, dtype=np.float64),
        image=np.asarray(image, dtype=np.float64),
        visibility=np.asarray(visibility, dtype=np.float64),
        detected=np.asarray(detected, dtype=bool),
        width=info.width,
        height=info.height,
        fps=float(media.FPS),
    )


def require_person(detected: np.ndarray) -> None:
    """Reject videos that do not really show a person.

    The detector occasionally "finds" a pose for a frame or two in footage
    with nobody in it. A handful of detections is not a performance, so the
    job fails with a clear message instead of producing a meaningless motion.
    """
    count, total = int(detected.sum()), len(detected)
    if count == 0:
        raise PermanentError("no person was detected in the video")
    if count < MIN_DETECTED_FRAMES or count < MIN_DETECTED_FRACTION * total:
        raise PermanentError(
            f"no person was detected for most of the video (only {count} of {total} frames); "
            "the video must show one person, head to feet, for most of its length"
        )


def fill_gaps(track: PoseTrack, max_gap: int = MAX_INTERPOLATED_GAP) -> PoseTrack:
    """Make the track continuous.

    - A run of fewer than max_gap missing frames is linearly interpolated
      between the detections on either side (at the very start or end of the
      clip, where there is only one side, it takes the nearest detected pose).
    - A longer run is recorded in track.gaps and the last known pose is held
      through it: a frozen pose is honest about the tracking loss, whereas
      interpolating across seconds would invent motion that never happened.
    """
    detected = track.detected
    if not detected.any():
        raise PermanentError("no person was detected in the video")
    frames = np.arange(len(detected))
    known = frames[detected]

    # Index of the nearest detected frame before and after each frame.
    before = np.maximum.accumulate(np.where(detected, frames, -1))
    after = np.minimum.accumulate(np.where(detected, frames, len(frames))[::-1])[::-1]

    gaps: list[tuple[int, int]] = []
    hold = np.zeros(len(frames), dtype=bool)  # frames to fill by holding, not interpolating
    start = None
    for i in range(len(frames) + 1):
        missing = i < len(frames) and not detected[i]
        if missing and start is None:
            start = i
        elif not missing and start is not None:
            if i - start >= max_gap:
                hold[start:i] = True
                gaps.append((start, i))
            start = None

    def fill(values: np.ndarray) -> np.ndarray:
        out = values.copy()
        flat = out.reshape(len(frames), -1)
        for column in range(flat.shape[1]):
            # np.interp does the linear interpolation for the short gaps (and
            # clamps to the first/last detection at the ends).
            flat[:, column] = np.interp(frames, known, flat[known, column])
        # Long gaps: overwrite the interpolation with the last pose before the gap.
        for i in frames[hold]:
            source = before[i] if before[i] >= 0 else after[i]
            out[i] = values[source]
        return out

    visibility = track.visibility.copy()
    visibility[~detected] = 0.0  # nothing was actually seen in a filled frame
    return PoseTrack(
        world=fill(track.world),
        image=fill(track.image),
        visibility=visibility,
        detected=detected,
        width=track.width,
        height=track.height,
        fps=track.fps,
        gaps=gaps,
    )


def one_euro(values: np.ndarray, rate: float, min_cutoff: float, beta: float, d_cutoff: float = 1.0) -> np.ndarray:
    """One Euro filter (Casiez et al., 2012) along axis 0.

    It is a low-pass filter whose cutoff frequency rises with speed. When a
    joint is nearly still the cutoff is low, which removes jitter; when it
    moves fast the cutoff is high, which avoids the lag a fixed low-pass filter
    would add. min_cutoff sets the smoothing at rest, beta how quickly the
    filter opens up with speed.
    """

    def alpha(cutoff: np.ndarray | float) -> np.ndarray | float:
        # Smoothing factor of a first-order low-pass at this cutoff frequency.
        tau = 1.0 / (2.0 * np.pi * cutoff)
        return 1.0 / (1.0 + tau * rate)

    out = np.empty_like(values, dtype=np.float64)
    out[0] = values[0]
    speed = np.zeros_like(values[0], dtype=np.float64)
    for t in range(1, len(values)):
        raw_speed = (values[t] - out[t - 1]) * rate
        a_d = alpha(d_cutoff)
        speed = a_d * raw_speed + (1 - a_d) * speed  # the speed estimate is itself low-passed
        a = alpha(min_cutoff + beta * np.abs(speed))
        out[t] = a * values[t] + (1 - a) * out[t - 1]
    return out


def smooth(values: np.ndarray, rate: float, min_cutoff: float = 2.0, beta: float = 1.0) -> np.ndarray:
    """Zero-lag smoothing for offline use.

    A causal filter always trails the signal slightly. Because the whole clip
    is available, the filter is run forwards and backwards and the two results
    are averaged: the forward lag and the backward lead cancel.
    """
    forward = one_euro(values, rate, min_cutoff, beta)
    backward = one_euro(values[::-1], rate, min_cutoff, beta)[::-1]
    return 0.5 * (forward + backward)
