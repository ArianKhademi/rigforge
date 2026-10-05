"""Smoke test: the whole pipeline (ffmpeg, MediaPipe, retargeting, writers) on
the 5-second sample clip, with nothing mocked. CI runs this and then validates
the output with the Khronos glTF validator.
"""

from __future__ import annotations

import json
import shutil
import subprocess
from pathlib import Path

import numpy as np
import pytest

from rigforge_worker import gltf, media, pipeline
from rigforge_worker.config import DEFAULT_MODEL_PATH
from rigforge_worker.errors import PermanentError

import gltf_eval
from conftest import unavailable
from synth import RIG_DIR

pytestmark = pytest.mark.pipeline

REPO = Path(__file__).resolve().parents[2]
SAMPLE = REPO / "docs" / "samples" / "power_jump.mp4"
VALIDATOR = REPO / "web" / "scripts" / "validate-gltf.mjs"


@pytest.fixture(scope="module")
def outputs(tmp_path_factory):
    if shutil.which("ffmpeg") is None:
        unavailable("ffmpeg is not installed")
    if not DEFAULT_MODEL_PATH.exists():
        unavailable(f"pose model missing at {DEFAULT_MODEL_PATH}; run `make models`")
    out_dir = tmp_path_factory.mktemp("pipeline")
    character = gltf.read_glb((RIG_DIR / "default_character.glb").read_bytes())
    stages: list[tuple[str, int]] = []
    result = pipeline.run(SAMPLE, out_dir, character, DEFAULT_MODEL_PATH, lambda s, p: stages.append((s, p)))
    return result, stages


def test_reports_metadata_for_the_sample(outputs):
    result, _ = outputs
    meta = result.metadata
    # The sample is 5.2 s at 29.97 fps; resampled to a constant 30 fps.
    assert meta["fps"] == 30.0
    assert meta["frame_count"] == pytest.approx(156, abs=2)
    assert meta["duration_seconds"] == pytest.approx(5.2, abs=0.1)
    assert (meta["width"], meta["height"]) == (720, 720)
    # One clearly visible person throughout: every frame should be detected.
    assert meta["detected_frames"] == meta["frame_count"]
    assert meta["gaps"] == 0


def test_progress_moves_through_the_stages_in_order(outputs):
    _, stages = outputs
    names = [name for name, _ in stages]
    order = [name for i, name in enumerate(names) if i == 0 or names[i - 1] != name]
    assert order == ["transcoding", "extracting", "writing"]
    percents = [pct for _, pct in stages]
    assert percents == sorted(percents), "progress must never go backwards"
    assert percents[0] == 0 and 85 <= percents[-1] <= 92


def test_preview_and_poster_are_playable(outputs):
    result, _ = outputs
    preview = media.probe(result.preview)
    # 480 px on the short side, same frame count as the motion.
    assert min(preview.width, preview.height) == 480
    assert preview.frame_count == result.metadata["frame_count"]
    assert result.poster.read_bytes()[:3] == b"\xff\xd8\xff"  # JPEG magic


def test_motion_glb_contains_the_character_and_one_keyframe_per_frame(outputs):
    result, _ = outputs
    glb = gltf.read_glb(result.motion_glb.read_bytes())
    assert len(glb.doc["meshes"]) == 1 and len(glb.doc["skins"]) == 1
    [animation] = glb.doc["animations"]
    assert len(animation["channels"]) == 18
    times = gltf_eval.accessor(glb, animation["samplers"][0]["input"])[:, 0]
    assert len(times) == result.metadata["frame_count"]
    assert np.allclose(np.diff(times), 1 / 30, atol=1e-5)
    assert animation["extras"]["detectedFrames"] == result.metadata["frame_count"]


def test_motion_looks_like_the_video(outputs):
    """The clip shows two power jumps: squat, jump with arms overhead, land.
    Check the solved motion for those three things rather than exact numbers."""
    result, _ = outputs
    glb = gltf.read_glb(result.motion_glb.read_bytes())
    names = ["hips", "head", "left_wrist", "right_wrist", "left_ankle", "right_ankle"]
    frames = result.metadata["frame_count"]
    track = np.array([gltf_eval.joint_positions(glb, names, f) for f in range(frames)])
    hips, head, wrists, ankles = track[:, 0], track[:, 1], track[:, 2:4], track[:, 4:6]

    # Squat: the hips drop to roughly half their standing height.
    assert hips[:, 1].max() > 0.9 and hips[:, 1].min() < 0.65
    # Arms overhead: at some point both wrists are well above the head.
    overhead = (wrists[:, :, 1] > head[:, None, 1] + 0.15).all(axis=1)
    assert overhead.any()
    # Jump: in those frames both feet are off the floor...
    assert ankles[overhead][:, :, 1].min() > 0.12
    # ...while for most of the clip the lower ankle is at standing height.
    grounded = np.median(ankles[:, :, 1].min(axis=1))
    assert 0.03 < grounded < 0.2
    # The performer stays in place, so the character should too.
    assert np.ptp(hips[:, 0]) < 0.25


def test_motion_bvh_has_one_line_per_frame(outputs):
    result, _ = outputs
    text = result.motion_bvh.read_text()
    assert f"Frames: {result.metadata['frame_count']}" in text
    motion_lines = text.split("Frame Time:")[1].strip().splitlines()[1:]
    assert len(motion_lines) == result.metadata["frame_count"]


def test_motion_glb_passes_the_khronos_validator(outputs):
    if shutil.which("node") is None or not (VALIDATOR.parent.parent / "node_modules" / "gltf-validator").exists():
        unavailable("glTF validator not installed; run `pnpm install` in web/")
    result, _ = outputs
    proc = subprocess.run(["node", str(VALIDATOR), "--json", str(result.motion_glb)], capture_output=True, text=True)
    report = json.loads(proc.stdout)
    assert report["issues"]["numErrors"] == 0, report["issues"]["messages"]
    assert report["issues"]["numWarnings"] == 0, report["issues"]["messages"]


def test_a_file_that_is_not_a_video_fails_permanently(tmp_path):
    if shutil.which("ffprobe") is None:
        unavailable("ffmpeg is not installed")
    bogus = tmp_path / "notes.mp4"
    bogus.write_text("this is not a video")
    with pytest.raises(PermanentError, match="not a readable video"):
        pipeline.run(bogus, tmp_path, None, DEFAULT_MODEL_PATH, lambda s, p: None)


def test_a_video_without_a_person_fails_permanently(tmp_path):
    if shutil.which("ffmpeg") is None or not DEFAULT_MODEL_PATH.exists():
        unavailable("ffmpeg or the pose model is missing")
    # One second of a test pattern: decodable, but nobody is in it.
    empty = tmp_path / "pattern.mp4"
    subprocess.run(
        ["ffmpeg", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30",
         "-t", "1", "-pix_fmt", "yuv420p", str(empty)],
        check=True,
    )  # fmt: skip
    with pytest.raises(PermanentError, match="no person"):
        pipeline.run(empty, tmp_path, None, DEFAULT_MODEL_PATH, lambda s, p: None)
