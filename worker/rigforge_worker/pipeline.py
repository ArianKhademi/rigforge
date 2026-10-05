"""The process_video job: source video in, motion asset out.

    download -> transcode -> extract pose -> retarget + write -> upload

Idempotent by construction: every output goes to a fixed key under
assets/{assetId}/, so running the same job again (a retry, or a duplicate
delivery) overwrites the same four objects and nothing else.
"""

from __future__ import annotations

import logging
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path

from . import bvh, gltf, media, pose, retarget
from .config import RIG_DIR
from .errors import PermanentError
from .skeleton import Skeleton, from_gltf, load_canonical
from .storage import ObjectStorage
from .worker import JobRow, ProgressFn

log = logging.getLogger("rigforge.pipeline")

CHARACTER_DEFAULT = "default"  # the bundled mannequin
CHARACTER_NONE = "none"  # skeleton-only output

# Each stage owns a slice of the 0-100 progress bar, sized roughly by how
# long it takes. The stage names are the job statuses the api exposes.
STAGES = {
    "transcoding": (0, 30),
    "extracting": (30, 85),
    "writing": (85, 92),
    "uploading": (92, 100),
}


@dataclass
class Outputs:
    motion_glb: Path
    motion_bvh: Path
    preview: Path
    poster: Path
    metadata: dict
    timings: dict[str, float]


def load_character(character_id: str, storage: ObjectStorage | None, tmp: Path) -> gltf.Glb | None:
    if character_id == CHARACTER_NONE:
        return None
    if character_id == CHARACTER_DEFAULT:
        return gltf.read_glb((RIG_DIR / "default_character.glb").read_bytes())
    if storage is None:
        raise PermanentError(f"character {character_id} is not available")
    path = tmp / "character.glb"
    storage.download(f"characters/{character_id}.glb", path)  # PermanentError if it was deleted
    return gltf.read_glb(path.read_bytes())


def run(source: Path, out_dir: Path, character: gltf.Glb | None, model_path: Path, progress: ProgressFn) -> Outputs:
    """Everything between download and upload, on local files. Split out so
    the smoke test and the CLI can run the real pipeline without any services.
    """
    timings: dict[str, float] = {}

    def stage(name: str):
        low, high = STAGES[name]
        progress(name, low)
        started = time.perf_counter()

        def report(fraction: float) -> None:
            progress(name, int(low + (high - low) * fraction))

        def done() -> None:
            timings[name] = round(time.perf_counter() - started, 3)

        return report, done

    # 1. Transcode. The working copy fixes resolution and frame rate so that
    #    everything downstream can assume 30 fps and at most 720p.
    report, done = stage("transcoding")
    source_info = media.probe(source)
    work, preview, poster = out_dir / "work.mp4", out_dir / "preview.mp4", out_dir / "poster.jpg"
    media.transcode(source, work, preview, source_info.duration, report)
    work_info = media.probe(work)
    media.poster(work, poster, at_seconds=min(1.0, work_info.duration / 2))
    done()

    # 2. Pose extraction, then clean-up of the raw track.
    report, done = stage("extracting")
    raw = pose.extract(work, model_path, report)
    track = pose.fill_gaps(raw)
    track.world = pose.smooth(track.world, track.fps)
    track.image = pose.smooth(track.image, track.fps)
    done()

    # 3. Retarget and write the asset files.
    report, done = stage("writing")
    canonical = load_canonical(RIG_DIR / "rigforge_skeleton.json")
    skeleton: Skeleton = from_gltf(character.doc, canonical) if character is not None else canonical
    motion = retarget.solve(track, skeleton)

    detected = int(raw.detected.sum())
    extras = {
        "generator": "rigforge",
        "sourceFrames": track.frame_count,
        "detectedFrames": detected,
        "cameraPitchDegrees": round(motion.camera_pitch_degrees, 2),
        # Frame ranges [start, end) where the subject was lost for too long to
        # interpolate; the pose is held through them.
        "gaps": [list(gap) for gap in track.gaps],
    }
    motion_glb, motion_bvh = out_dir / "motion.glb", out_dir / "motion.bvh"
    motion_glb.write_bytes(gltf.build_motion_glb(skeleton, motion, character, extras))
    motion_bvh.write_text(bvh.write_bvh(skeleton, motion))
    done()

    metadata = {
        "duration_seconds": round(track.frame_count / track.fps, 3),
        "width": source_info.width,
        "height": source_info.height,
        "fps": track.fps,
        "frame_count": track.frame_count,
        "detected_frames": detected,
        "gaps": len(track.gaps),
    }
    return Outputs(motion_glb, motion_bvh, preview, poster, metadata, timings)


def process_video(
    job: JobRow, progress: ProgressFn, storage: ObjectStorage, model_path: Path, work_dir: str | None
) -> dict:
    """The queue handler. Raises PermanentError for bad input, anything else
    is treated as transient by the worker loop and retried."""
    # TemporaryDirectory removes the source and all intermediates on exit,
    # including when the job fails, so a worker cannot fill its disk.
    with tempfile.TemporaryDirectory(prefix=f"rigforge-{job.id}-", dir=work_dir) as tmp_name:
        tmp = Path(tmp_name)
        progress("transcoding", 0)
        source = tmp / "source"
        storage.download(job.source_key, source)
        character = load_character(job.character_id, storage, tmp)

        outputs = run(source, tmp, character, model_path, progress)

        low, high = STAGES["uploading"]
        files = [outputs.preview, outputs.poster, outputs.motion_bvh, outputs.motion_glb]
        for i, path in enumerate(files):
            progress("uploading", int(low + (high - low) * i / len(files)))
            storage.upload(path, f"assets/{job.asset_id}/{path.name}")
        log.info("job %s processed: %s timings=%s", job.id, outputs.metadata, outputs.timings)
        return outputs.metadata
