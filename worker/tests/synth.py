"""Synthetic performers for the retargeting tests.

A test poses a skeleton with known rotations, turns the posed skeleton into
the 33 landmarks MediaPipe would report for it (including the camera's view
of it), and then checks that the solver recovers the pose. Nothing here uses
the solver's own code path for the inverse direction except forward
kinematics, which is itself checked against bone lengths in the tests.
"""

from __future__ import annotations

from pathlib import Path

import numpy as np

from rigforge_worker import retarget as rt
from rigforge_worker.pose import PoseTrack
from rigforge_worker.skeleton import Skeleton, load_canonical

RIG_DIR = Path(__file__).resolve().parents[1] / "rig"
IDENTITY = np.array([0.0, 0.0, 0.0, 1.0])

# A simple orthographic camera for the image landmarks.
WIDTH, HEIGHT = 1280, 720
PIXELS_PER_METRE = 300.0
FLOOR_ROW = 680.0  # image row of y = 0


def canonical() -> Skeleton:
    return load_canonical(RIG_DIR / "rigforge_skeleton.json")


def axis_angle(axis, degrees: float) -> np.ndarray:
    axis = np.asarray(axis, dtype=float)
    axis = axis / np.linalg.norm(axis)
    half = np.radians(degrees) / 2.0
    return np.concatenate([axis * np.sin(half), [np.cos(half)]])


def angle_degrees(q: np.ndarray) -> np.ndarray:
    """Rotation angle of a quaternion, 0..180 degrees."""
    return np.degrees(2.0 * np.arccos(np.clip(np.abs(q[..., 3]), 0.0, 1.0)))


def pose(skeleton: Skeleton, local: dict[str, np.ndarray] | None = None, frames: int = 1) -> np.ndarray:
    """World delta rotations (frames, J, 4) for a pose given as local joint
    rotations by name (unnamed joints stay at their bind rotation)."""
    local = local or {}
    world = np.tile(IDENTITY, (len(skeleton.names), 1))
    for j, parent in enumerate(skeleton.parents):
        q = local.get(skeleton.names[j], IDENTITY)
        world[j] = q if parent < 0 else rt.quat_mul(world[parent], q)
    return np.tile(world, (frames, 1, 1))


def performer(
    skeleton: Skeleton,
    world_delta: np.ndarray,
    root: np.ndarray | None = None,
    camera_pitch_degrees: float = 0.0,
    eye_bias_degrees: float = 0.0,
    fps: float = 30.0,
) -> PoseTrack:
    """The PoseTrack MediaPipe would produce for a posed skeleton.

    root                  (T, 3) root joint positions; default: bind position
    camera_pitch_degrees  tilt the camera down by this much
    eye_bias_degrees      raise the eye landmarks above the ear line, as
                          MediaPipe's face landmarks do
    """
    T = world_delta.shape[0]
    idx = skeleton.index
    if root is None:
        root = np.tile(skeleton.bind_pos[idx("hips")], (T, 1))
    joints, tips = rt.forward_kinematics(skeleton, world_delta, root)
    P = skeleton.bind_pos

    points = np.zeros((T, 33, 3))
    for side, (shoulder, elbow, wrist, pinky, index, thumb, hip, knee, ankle, heel, foot) in {
        "left": (11, 13, 15, 17, 19, 21, 23, 25, 27, 29, 31),
        "right": (12, 14, 16, 18, 20, 22, 24, 26, 28, 30, 32),
    }.items():
        for landmark, part in (
            (shoulder, "shoulder"),
            (elbow, "elbow"),
            (wrist, "wrist"),
            (hip, "hip"),
            (knee, "knee"),
            (ankle, "ankle"),
        ):
            points[:, landmark] = joints[:, idx(f"{side}_{part}")]
        w, a = idx(f"{side}_wrist"), idx(f"{side}_ankle")
        # Knuckles either side of the hand's tip, so their midpoint is the tip.
        spread = rt.quat_rotate(world_delta[:, w], np.array([0.0, 0.0, 0.03]))
        points[:, index] = tips[w] + spread
        points[:, pinky] = tips[w] - spread
        points[:, thumb] = tips[w] + spread
        points[:, foot] = tips[a]
        # Heel: on the floor directly under the ankle (in the bind pose).
        points[:, heel] = joints[:, a] + rt.quat_rotate(world_delta[:, a], np.array([0.0, -P[a][1], 0.0]))

    head = idx("head")
    lift = 0.085 * np.tan(np.radians(eye_bias_degrees))

    def on_head(offset):
        return joints[:, head] + rt.quat_rotate(world_delta[:, head], np.array(offset))

    points[:, rt.L_EAR], points[:, rt.R_EAR] = on_head([0.075, 0.10, 0.0]), on_head([-0.075, 0.10, 0.0])
    points[:, rt.L_EYE], points[:, rt.R_EYE] = on_head([0.03, 0.10 + lift, 0.085]), on_head([-0.03, 0.10 + lift, 0.085])
    points[:, 0] = on_head([0.0, 0.07, 0.11])  # nose

    # Image landmarks: an orthographic projection of the scene.
    image = np.zeros((T, 33, 2))
    image[..., 0] = WIDTH / 2.0 + points[..., 0] * PIXELS_PER_METRE
    image[..., 1] = FLOOR_ROW - points[..., 1] * PIXELS_PER_METRE

    # World landmarks: centred on the hips, seen from a camera pitched down
    # (the scene appears rotated about X towards the camera), in MediaPipe's
    # axes (y down, z away), which is the inverse of to_gltf_space.
    hip_centre = 0.5 * (points[:, rt.L_HIP] + points[:, rt.R_HIP])
    centred = points - hip_centre[:, None, :]
    world = rt.level(centred, -np.radians(camera_pitch_degrees)) * np.array([1.0, -1.0, -1.0])

    return PoseTrack(
        world=world,
        image=image,
        visibility=np.ones((T, 33)),
        detected=np.ones(T, dtype=bool),
        width=WIDTH,
        height=HEIGHT,
        fps=fps,
    )
