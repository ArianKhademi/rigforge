"""ffprobe / ffmpeg wrappers. Everything goes through subprocess with argument
lists (never a shell string), and video data only ever moves between files and
pipes: no step loads a whole video into memory.
"""

from __future__ import annotations

import json
import subprocess
from collections.abc import Callable, Iterator
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .errors import PermanentError

# The working copy and everything derived from it (preview, pose samples,
# animation keyframes) share one constant frame rate, so frame i is at i / FPS
# seconds in all of them. That shared clock is what keeps the preview video
# and the motion in sync in the UI.
FPS = 30


@dataclass(frozen=True)
class VideoInfo:
    width: int
    height: int
    duration: float  # seconds
    frame_count: int


def _run(args: list[str], what: str) -> subprocess.CompletedProcess:
    proc = subprocess.run(args, capture_output=True, text=True)
    if proc.returncode != 0:
        tail = "\n".join(proc.stderr.strip().splitlines()[-5:])
        raise RuntimeError(f"{what} failed (exit {proc.returncode}): {tail}")
    return proc


def probe(path: Path) -> VideoInfo:
    """Read basic metadata with ffprobe. Raises PermanentError if the file is
    not a decodable video: no retry will change that."""
    proc = subprocess.run(
        ["ffprobe", "-v", "error", "-print_format", "json", "-show_format", "-show_streams",
         "-count_packets", "-select_streams", "v:0", str(path)],
        capture_output=True, text=True,
    )  # fmt: skip
    if proc.returncode != 0:
        raise PermanentError("the uploaded file is not a readable video")
    data = json.loads(proc.stdout or "{}")
    streams = data.get("streams") or []
    if not streams:
        raise PermanentError("the uploaded file has no video stream")
    stream = streams[0]
    duration = float(stream.get("duration") or data.get("format", {}).get("duration") or 0.0)
    if duration <= 0:
        raise PermanentError("the uploaded video has no duration")
    return VideoInfo(
        width=int(stream["width"]),
        height=int(stream["height"]),
        duration=duration,
        frame_count=int(stream.get("nb_read_packets") or stream.get("nb_frames") or 0),
    )


def transcode(src: Path, work: Path, preview: Path, duration: float, on_progress: Callable[[float], None]) -> None:
    """One ffmpeg run, one decode of the source, two encodes:

    work     the copy pose extraction reads: constant 30 fps, H.264, short
             side at most 720 px
    preview  what the browser plays next to the 3D view: 480 px, smaller
             bitrate, moov atom up front so it starts playing before it has
             fully downloaded

    Decoding a multi-gigabyte upload once instead of twice is the reason both
    outputs come from a single filter graph.
    """

    # Scale so the shorter side is at most N, never upscaling. -2 keeps the
    # aspect ratio and rounds to an even number (H.264 with yuv420p needs even
    # dimensions). Phone videos carry a rotation flag; ffmpeg applies it before
    # the filters, so "iw"/"ih" here are already the displayed orientation.
    def short_side(n: int) -> str:
        return f"scale='if(gt(iw,ih),-2,min({n},iw))':'if(gt(iw,ih),min({n},ih),-2)'"

    graph = (
        f"[0:v]fps={FPS},split=2[a][b];"
        f"[a]{short_side(720)}[work];"
        f"[b]{short_side(480)}[preview]"
    )  # fmt: skip
    args = [
        "ffmpeg", "-hide_banner", "-nostdin", "-y", "-loglevel", "error",
        # Machine-readable progress on stdout: "out_time_us=1234567" lines.
        "-progress", "pipe:1", "-nostats",
        "-i", str(src),
        "-filter_complex", graph,
        "-map", "[work]", "-an",
        "-c:v", "libx264", "-preset", "veryfast", "-crf", "20", "-pix_fmt", "yuv420p",
        str(work),
        "-map", "[preview]", "-map", "0:a?",  # "?" = keep audio only if the source has any
        "-c:v", "libx264", "-preset", "veryfast", "-crf", "26", "-pix_fmt", "yuv420p",
        "-c:a", "aac", "-b:a", "96k",
        "-movflags", "+faststart",
        str(preview),
    ]  # fmt: skip
    proc = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    assert proc.stdout is not None and proc.stderr is not None
    for line in proc.stdout:
        key, _, value = line.strip().partition("=")
        if key == "out_time_us" and value.isdigit():
            on_progress(min(1.0, int(value) / 1e6 / duration))
    stderr = proc.stderr.read()
    if proc.wait() != 0:
        tail = "\n".join(stderr.strip().splitlines()[-5:])
        # ffmpeg could open the file (ffprobe passed) but not decode it.
        raise PermanentError(f"ffmpeg could not transcode the video: {tail}")


def poster(video: Path, dest: Path, at_seconds: float) -> None:
    """Grab one frame as a JPEG for the browse grid."""
    _run(
        ["ffmpeg", "-hide_banner", "-nostdin", "-y", "-loglevel", "error",
         "-ss", f"{at_seconds:.3f}", "-i", str(video), "-frames:v", "1", "-q:v", "3", str(dest)],
        "poster frame",
    )  # fmt: skip


def read_frames(video: Path, width: int, height: int) -> Iterator[np.ndarray]:
    """Yield decoded frames one at a time as (height, width, 3) uint8 RGB.

    ffmpeg writes raw pixels to a pipe and we read exactly one frame's worth
    of bytes per iteration, so memory use is one frame regardless of how long
    the video is.
    """
    frame_bytes = width * height * 3
    proc = subprocess.Popen(
        ["ffmpeg", "-hide_banner", "-nostdin", "-loglevel", "error",
         "-i", str(video), "-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1"],
        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, bufsize=frame_bytes,
    )  # fmt: skip
    assert proc.stdout is not None
    try:
        while True:
            buf = proc.stdout.read(frame_bytes)
            if len(buf) < frame_bytes:
                break
            yield np.frombuffer(buf, dtype=np.uint8).reshape(height, width, 3)
    finally:
        proc.stdout.close()
        proc.kill()
        proc.wait()
