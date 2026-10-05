"""BVH export of a solved motion.

BVH is the lowest common denominator of motion-capture formats: a text file
with a joint hierarchy (offsets only, no rest rotations) followed by one line
of channel values per frame. Blender, Maya, MotionBuilder and Unity tooling
all read it.
"""

from __future__ import annotations

import numpy as np

from .retarget import Motion, local_rotations, quat_to_matrix
from .skeleton import Skeleton

# BVH has no unit field; centimetres is the common convention.
CM_PER_M = 100.0


def euler_zxy_degrees(quats: np.ndarray) -> tuple[np.ndarray, np.ndarray, np.ndarray]:
    """Decompose rotations into Z, X, Y Euler angles (degrees) such that
    R = Rz * Rx * Ry, the order the CHANNELS line below declares.

    Multiplying the three axis rotations out gives
        R[2][1] = sin(x)
        R[0][1] = -sin(z) cos(x)      R[1][1] = cos(z) cos(x)
        R[2][0] = -cos(x) sin(y)      R[2][2] = cos(x) cos(y)
    from which each angle can be read back with asin / atan2.
    """
    m = quat_to_matrix(quats)
    x = np.arcsin(np.clip(m[..., 2, 1], -1.0, 1.0))
    z = np.arctan2(-m[..., 0, 1], m[..., 1, 1])
    y = np.arctan2(-m[..., 2, 0], m[..., 2, 2])
    return np.degrees(z), np.degrees(x), np.degrees(y)


def write_bvh(skeleton: Skeleton, motion: Motion) -> str:
    lines: list[str] = ["HIERARCHY"]
    order: list[int] = []  # joints in the order their channels appear in each frame line

    def emit(joint: int, depth: int) -> None:
        pad = "  " * depth
        parent = skeleton.parents[joint]
        # Offsets are world-space differences of bind positions: a BVH rest
        # pose has no rotations, every joint's axes are the world axes.
        origin = skeleton.bind_pos[parent] if parent >= 0 else np.zeros(3)
        offset = (skeleton.bind_pos[joint] - origin) * CM_PER_M
        lines.append(f"{pad}{'ROOT' if parent < 0 else 'JOINT'} {skeleton.names[joint]}")
        lines.append(f"{pad}{{")
        lines.append(f"{pad}  OFFSET {offset[0]:.4f} {offset[1]:.4f} {offset[2]:.4f}")
        if parent < 0:
            lines.append(f"{pad}  CHANNELS 6 Xposition Yposition Zposition Zrotation Xrotation Yrotation")
        else:
            lines.append(f"{pad}  CHANNELS 3 Zrotation Xrotation Yrotation")
        order.append(joint)
        for child in skeleton.children(joint):
            emit(child, depth + 1)
        if joint in skeleton.ends:  # a leaf: give its bone a length with an End Site
            tip = (skeleton.ends[joint] - skeleton.bind_pos[joint]) * CM_PER_M
            lines.append(f"{pad}  End Site")
            lines.append(f"{pad}  {{")
            lines.append(f"{pad}    OFFSET {tip[0]:.4f} {tip[1]:.4f} {tip[2]:.4f}")
            lines.append(f"{pad}  }}")
        lines.append(f"{pad}}}")

    emit(skeleton.parents.index(-1), 0)

    frames = len(motion.times)
    frame_time = float(motion.times[1] - motion.times[0]) if frames > 1 else 1.0 / 30.0
    lines += ["MOTION", f"Frames: {frames}", f"Frame Time: {frame_time:.6f}"]

    # identity_bind: BVH joints have no rest rotation (see emit above).
    z, x, y = euler_zxy_degrees(local_rotations(motion.world_delta, skeleton, identity_bind=True))
    root = motion.root_pos * CM_PER_M
    for t in range(frames):
        values = [f"{root[t, 0]:.4f}", f"{root[t, 1]:.4f}", f"{root[t, 2]:.4f}"]
        for joint in order:
            values += [f"{z[t, joint]:.4f}", f"{x[t, joint]:.4f}", f"{y[t, joint]:.4f}"]
        lines.append(" ".join(values))
    return "\n".join(lines) + "\n"
