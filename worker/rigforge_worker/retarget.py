"""Retargeting: turn pose landmarks into an animation of the Rigforge skeleton.

Input   33 landmark positions per frame (MediaPipe "world" landmarks).
Output  one rotation per skeleton joint per frame, plus the root position.

Why rotations and not positions: a skinned character is driven by joint
rotations. Rotations also make the result independent of body proportions: a
tall performer and a short character share the same "elbow bent 90 degrees".

THE IDEA IN ONE PARAGRAPH
For every joint j we compute a world-space *delta* rotation D[j]: the rotation
that takes the bone from where it points in the bind pose (the T-pose) to
where the landmarks say it points now. In the bind pose every D is the
identity. The torso joints get D from a full 3-axis frame built from the hip
and shoulder landmarks; the limb joints get D by swinging the parent's
rotation the shortest way onto the observed bone direction. Converting D to
the parent-relative ("local") rotations that glTF stores is then one line:
local[j] = inverse(W[parent]) * W[j] with W[j] = D[j] * bind[j].

CONVENTIONS
- Quaternions are [x, y, z, w] (glTF order), unit length, and every function
  here accepts arrays with any leading batch shape, e.g. (T, 4) for T frames.
- quat_mul(a, b) means "apply b, then a", like matrix multiplication.
- Space is glTF's: metres, +Y up, the character faces +Z, +X is its LEFT.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import TYPE_CHECKING

import numpy as np

if TYPE_CHECKING:  # imported for type hints only; skeleton.py imports this module
    from .pose import PoseTrack
    from .skeleton import Skeleton

EPS = 1e-9

# MediaPipe Pose landmark indices that the solver uses.
L_EYE, R_EYE = 2, 5
L_EAR, R_EAR = 7, 8
L_SHOULDER, R_SHOULDER = 11, 12
L_ELBOW, R_ELBOW = 13, 14
L_WRIST, R_WRIST = 15, 16
L_PINKY, R_PINKY = 17, 18
L_INDEX, R_INDEX = 19, 20
L_HIP, R_HIP = 23, 24
L_KNEE, R_KNEE = 25, 26
L_ANKLE, R_ANKLE = 27, 28
L_HEEL, R_HEEL = 29, 30
L_FOOT, R_FOOT = 31, 32  # "foot index": the tip of the toes


# --------------------------------------------------------------------------
# Quaternion and vector helpers
# --------------------------------------------------------------------------


def normalize(v: np.ndarray) -> np.ndarray:
    return v / np.maximum(np.linalg.norm(v, axis=-1, keepdims=True), EPS)


def quat_mul(a: np.ndarray, b: np.ndarray) -> np.ndarray:
    """Hamilton product: the rotation "b first, then a"."""
    ax, ay, az, aw = np.moveaxis(np.asarray(a, dtype=np.float64), -1, 0)
    bx, by, bz, bw = np.moveaxis(np.asarray(b, dtype=np.float64), -1, 0)
    return np.stack(
        [
            aw * bx + ax * bw + ay * bz - az * by,
            aw * by - ax * bz + ay * bw + az * bx,
            aw * bz + ax * by - ay * bx + az * bw,
            aw * bw - ax * bx - ay * by - az * bz,
        ],
        axis=-1,
    )


def quat_conj(q: np.ndarray) -> np.ndarray:
    """Inverse of a unit quaternion."""
    return np.asarray(q, dtype=np.float64) * np.array([-1.0, -1.0, -1.0, 1.0])


def quat_rotate(q: np.ndarray, v: np.ndarray) -> np.ndarray:
    """Rotate vector v by q, using v' = v + 2w(u x v) + 2u x (u x v)."""
    q = np.asarray(q, dtype=np.float64)
    u, w = q[..., :3], q[..., 3:4]
    t = 2.0 * np.cross(u, v)
    return v + w * t + np.cross(u, t)


def quat_between(a: np.ndarray, b: np.ndarray) -> np.ndarray:
    """Shortest-arc rotation taking unit vector a onto unit vector b.

    The axis is a x b and, with c = a . b, the unnormalised quaternion is
    [a x b, 1 + c]. This is the rotation with no twist about the result: it
    moves a onto b and does nothing else.
    """
    a, b = np.broadcast_arrays(np.asarray(a, dtype=np.float64), np.asarray(b, dtype=np.float64))
    q = np.concatenate([np.cross(a, b), 1.0 + np.sum(a * b, axis=-1, keepdims=True)], axis=-1)
    # a and b opposite: the formula degenerates to [0, 0, 0, 0]. Any axis
    # perpendicular to a gives a valid half-turn; pick one deterministically.
    opposite = q[..., 3] < 1e-8
    if np.any(opposite):
        helper = np.where(np.abs(a[..., :1]) < 0.9, [1.0, 0.0, 0.0], [0.0, 1.0, 0.0])
        axis = normalize(np.cross(a, helper))
        q = np.where(opposite[..., None], np.concatenate([axis, np.zeros_like(axis[..., :1])], axis=-1), q)
    return normalize(q)


def quat_slerp(a: np.ndarray, b: np.ndarray, t: np.ndarray | float) -> np.ndarray:
    """Spherical interpolation from a (t = 0) to b (t = 1)."""
    a, b = np.broadcast_arrays(np.asarray(a, dtype=np.float64), np.asarray(b, dtype=np.float64))
    t = np.asarray(t, dtype=np.float64)[..., None]
    dot = np.sum(a * b, axis=-1, keepdims=True)
    # q and -q are the same rotation; flip b so we take the short way round.
    b = np.where(dot < 0, -b, b)
    dot = np.abs(dot)
    theta = np.arccos(np.clip(dot, -1.0, 1.0))
    sin_theta = np.sin(theta)
    # Nearly identical rotations: sin(theta) ~ 0, fall back to linear weights.
    close = sin_theta < 1e-6
    safe = np.where(close, 1.0, sin_theta)
    wa = np.where(close, 1.0 - t, np.sin((1.0 - t) * theta) / safe)
    wb = np.where(close, t, np.sin(t * theta) / safe)
    return normalize(wa * a + wb * b)


def quat_to_matrix(q: np.ndarray) -> np.ndarray:
    x, y, z, w = np.moveaxis(np.asarray(q, dtype=np.float64), -1, 0)
    return np.stack(
        [
            np.stack([1 - 2 * (y * y + z * z), 2 * (x * y - z * w), 2 * (x * z + y * w)], axis=-1),
            np.stack([2 * (x * y + z * w), 1 - 2 * (x * x + z * z), 2 * (y * z - x * w)], axis=-1),
            np.stack([2 * (x * z - y * w), 2 * (y * z + x * w), 1 - 2 * (x * x + y * y)], axis=-1),
        ],
        axis=-2,
    )


def quat_from_matrix(m: np.ndarray) -> np.ndarray:
    """Rotation matrix (..., 3, 3) to quaternion.

    From the matrix entries one can form four vectors, each equal to the
    quaternion scaled by 4 times one of its own components (x, y, z or w).
    Dividing by a component that is close to zero would be unstable, so we use
    the row whose scaling component is largest.
    """
    m = np.asarray(m, dtype=np.float64)
    m00, m01, m02 = m[..., 0, 0], m[..., 0, 1], m[..., 0, 2]
    m10, m11, m12 = m[..., 1, 0], m[..., 1, 1], m[..., 1, 2]
    m20, m21, m22 = m[..., 2, 0], m[..., 2, 1], m[..., 2, 2]
    rows = np.stack(
        [
            np.stack([1 + m00 - m11 - m22, m01 + m10, m02 + m20, m21 - m12], axis=-1),  # 4x * q
            np.stack([m01 + m10, 1 - m00 + m11 - m22, m12 + m21, m02 - m20], axis=-1),  # 4y * q
            np.stack([m02 + m20, m12 + m21, 1 - m00 - m11 + m22, m10 - m01], axis=-1),  # 4z * q
            np.stack([m21 - m12, m02 - m20, m10 - m01, 1 + m00 + m11 + m22], axis=-1),  # 4w * q
        ],
        axis=-2,
    )
    diagonal = np.stack([rows[..., i, i] for i in range(4)], axis=-1)
    best = np.argmax(diagonal, axis=-1)
    chosen = np.take_along_axis(rows, best[..., None, None], axis=-2)[..., 0, :]
    return normalize(chosen)


def frame(left: np.ndarray, up: np.ndarray) -> np.ndarray:
    """Orthonormal basis as a rotation matrix with columns [x, y, z].

    x is exactly along `left`. `up` only needs to be roughly upward: z (forward)
    is made perpendicular to both, and y is then recomputed so the three axes
    are exactly perpendicular. x cross y = z, a right-handed frame.
    """
    x = normalize(left)
    z = normalize(np.cross(x, up))
    y = np.cross(z, x)
    return np.stack([x, y, z], axis=-1)


# --------------------------------------------------------------------------
# Landmarks -> world delta rotations
# --------------------------------------------------------------------------


def to_gltf_space(mediapipe_points: np.ndarray) -> np.ndarray:
    """MediaPipe world landmarks have x right, y DOWN, z AWAY from the camera.
    Negating y and z (a half-turn about x, so handedness is preserved) gives
    glTF's y up, z towards the viewer. A person facing the camera then faces
    +Z with their left side at +X, exactly the skeleton's bind orientation.
    """
    return np.asarray(mediapipe_points, dtype=np.float64) * np.array([1.0, -1.0, -1.0])


def body_line(points: np.ndarray) -> np.ndarray:
    """Vector from the foot of the supporting leg to the shoulder centre, per
    frame, with the leg shifted sideways onto the body's midline.

    The supporting leg is taken to be the straighter one (larger hip-to-ankle
    distance), so a lifted or bent leg does not skew the line. Moving it from
    its own hip to the hip centre removes the half-hip-width offset, which
    would otherwise read as a lean when the performer is seen in profile.
    """
    left = points[:, L_ANKLE] - points[:, L_HIP]
    right = points[:, R_ANKLE] - points[:, R_HIP]
    use_left = np.linalg.norm(left, axis=-1) >= np.linalg.norm(right, axis=-1)
    hip_centre = 0.5 * (points[:, L_HIP] + points[:, R_HIP])
    foot = hip_centre + np.where(use_left[:, None], left, right)
    return 0.5 * (points[:, L_SHOULDER] + points[:, R_SHOULDER]) - foot


def standing_tall(points: np.ndarray) -> np.ndarray:
    """Boolean mask of the frames where the performer is at full height.

    Detected without knowing which way is up: those are the frames where the
    body line is close to its maximum length over the clip. Two calibrations
    below rely on them, on the assumption that the performer stands upright at
    some point.
    """
    extension = np.linalg.norm(body_line(points), axis=-1)
    return extension >= 0.9 * np.percentile(extension, 95)


def estimate_camera_pitch(points: np.ndarray) -> float:
    """Estimate how far the camera is tilted down (+) or up (-), in radians.

    MediaPipe reports landmarks in the camera's frame, not gravity's. A camera
    on a tripod looking slightly down at the performer therefore makes them
    appear to lean towards it, all the time. That constant lean is the camera's
    pitch, and it can be measured on the frames where the performer stands
    tall: there the body line should be vertical, so whatever forward tilt it
    shows is the camera's.
    """
    line = body_line(points)[standing_tall(points)]
    # Angle of the body line from vertical, measured in the Y-Z (side) plane.
    pitch = float(np.median(np.arctan2(line[:, 2], line[:, 1])))
    # A real camera tilt is modest; beyond this the "lean" is the performance.
    limit = np.radians(30.0)
    return float(np.clip(pitch, -limit, limit))


def level(points: np.ndarray, pitch: float) -> np.ndarray:
    """Rotate landmarks about the X (left-right) axis by -pitch, undoing the
    camera tilt so that "up" in the data is up in the world."""
    c, s = np.cos(pitch), np.sin(pitch)
    out = points.copy()
    out[..., 1] = c * points[..., 1] + s * points[..., 2]
    out[..., 2] = -s * points[..., 1] + c * points[..., 2]
    return out


def _confidence(visibility: np.ndarray, *landmarks: int) -> np.ndarray:
    """Map the lowest visibility among some landmarks to a 0..1 blend weight:
    0 below 0.3 (do not trust), 1 above 0.7 (trust fully), linear in between.
    A smooth ramp avoids a visible pop when a landmark crosses a threshold."""
    lowest = np.min(visibility[..., list(landmarks)], axis=-1)
    return np.clip((lowest - 0.3) / 0.4, 0.0, 1.0)


def solve_rotations(points: np.ndarray, visibility: np.ndarray, skeleton: Skeleton) -> np.ndarray:
    """Compute the world delta rotation D of every joint for every frame.

    points      (T, 33, 3) landmarks in glTF space
    visibility  (T, 33)
    returns     (T, J, 4) quaternions; D[t, j] rotates joint j's bind-pose
                orientation into its orientation at frame t, in world space.
    """
    T = points.shape[0]
    J = len(skeleton.names)
    P = skeleton.bind_pos
    idx = skeleton.index
    D = np.zeros((T, J, 4))

    def lm(i: int) -> np.ndarray:
        return points[:, i]

    # ---- Torso: full frames from pairs of landmarks ----------------------
    # A single bone direction leaves the twist about that bone undetermined.
    # The torso has enough landmarks to pin all three axes: the hip line gives
    # left/right, and hip centre -> shoulder centre gives up.
    hip_centre = 0.5 * (lm(L_HIP) + lm(R_HIP))
    shoulder_centre = 0.5 * (lm(L_SHOULDER) + lm(R_SHOULDER))
    spine_up = shoulder_centre - hip_centre

    pelvis_now = frame(lm(L_HIP) - lm(R_HIP), spine_up)
    chest_now = frame(lm(L_SHOULDER) - lm(R_SHOULDER), spine_up)

    # The same two frames measured on the skeleton's bind pose.
    bind_hip_centre = 0.5 * (P[idx("left_hip")] + P[idx("right_hip")])
    bind_shoulder_centre = 0.5 * (P[idx("left_shoulder")] + P[idx("right_shoulder")])
    bind_up = bind_shoulder_centre - bind_hip_centre
    pelvis_bind = frame(P[idx("left_hip")] - P[idx("right_hip")], bind_up)
    chest_bind = frame(P[idx("left_shoulder")] - P[idx("right_shoulder")], bind_up)

    # now = D * bind  =>  D = now * bind^-1, and for a rotation matrix the
    # inverse is the transpose.
    D[:, idx("hips")] = quat_from_matrix(pelvis_now @ pelvis_bind.T)
    D[:, idx("chest")] = quat_from_matrix(chest_now @ chest_bind.T)
    # There is no landmark between hips and shoulders, so the spine joint takes
    # half of the rotation between the two, which spreads a torso twist over
    # the spine instead of creasing it at one joint.
    D[:, idx("spine")] = quat_slerp(D[:, idx("hips")], D[:, idx("chest")], 0.5)

    # ---- Head: a frame from the ears and eyes ----------------------------
    # Left/right is the ear line; forward is ear centre -> eye centre.
    ear_line = lm(L_EAR) - lm(R_EAR)
    forward = 0.5 * (lm(L_EYE) + lm(R_EYE)) - 0.5 * (lm(L_EAR) + lm(R_EAR))
    head_now = frame(ear_line, np.cross(forward, ear_line))  # forward x left = up

    # Calibration. The ear-to-eye line is not level on a level head: MediaPipe
    # places the eyes well above the ears (about 25 degrees on test footage).
    # Rather than hard-code that, measure it on this clip: the head's pitch
    # relative to the chest, on the frames where the performer stands tall, is
    # taken as "looking straight ahead" and removed. Head pitch is therefore
    # relative to the performer's own neutral posture.
    forward_in_chest = np.einsum("tji,tj->ti", chest_now, normalize(forward))  # chest_now^T * forward
    neutral = float(np.median(np.arctan2(forward_in_chest[:, 1], forward_in_chest[:, 2])[standing_tall(points)]))
    c, s = np.cos(neutral), np.sin(neutral)
    # Rotation about the head's own left-right axis (applied on the right, so
    # in the head's local frame) that tips "forward" back down by `neutral`.
    head_now = head_now @ np.array([[1.0, 0.0, 0.0], [0.0, c, -s], [0.0, s, c]])

    # The rig has no ear or eye joints, so the head's bind frame is taken to be
    # "level, facing the same way as the shoulders".
    head_bind = frame(P[idx("left_shoulder")] - P[idx("right_shoulder")], np.array([0.0, 1.0, 0.0]))
    head_measured = quat_from_matrix(head_now @ head_bind.T)
    # Face landmarks are unreliable when the head is turned away or occluded;
    # fade towards "head follows the chest" as their visibility drops.
    trust = _confidence(visibility, L_EAR, R_EAR, L_EYE, R_EYE)
    D[:, idx("head")] = quat_slerp(D[:, idx("chest")], head_measured, trust)
    D[:, idx("neck")] = quat_slerp(D[:, idx("chest")], D[:, idx("head")], 0.5)

    # ---- Limbs: swing the parent's rotation onto the observed bone -------
    def swing(joint: str, parent: str, bind_from: np.ndarray, bind_to: np.ndarray,
              now_from: np.ndarray, now_to: np.ndarray, trust: np.ndarray | None = None) -> None:  # fmt: skip
        """Aim the bone that starts at `joint`.

        If the joint did not bend at all it would simply carry its parent's
        rotation, and the bone would point along D[parent] * bind_direction.
        The landmarks say where it really points. The shortest-arc rotation
        between those two directions is the bend at this joint; composing it
        with the parent's rotation gives D[joint]. Shortest-arc means "bend,
        without adding twist", which is the natural default when a single
        direction cannot tell us the twist.
        """
        parent_d = D[:, idx(parent)]
        inherited = quat_rotate(parent_d, normalize(bind_to - bind_from))
        observed = normalize(now_to - now_from)
        bent = quat_mul(quat_between(inherited, observed), parent_d)
        if trust is not None:
            bent = quat_slerp(parent_d, bent, trust)
        D[:, idx(joint)] = bent

    for side, shoulder, elbow, wrist, index, pinky, hip, knee, ankle, foot in (
        ("left", L_SHOULDER, L_ELBOW, L_WRIST, L_INDEX, L_PINKY, L_HIP, L_KNEE, L_ANKLE, L_FOOT),
        ("right", R_SHOULDER, R_ELBOW, R_WRIST, R_INDEX, R_PINKY, R_HIP, R_KNEE, R_ANKLE, R_FOOT),
    ):
        j = {part: idx(f"{side}_{part}") for part in ("shoulder", "elbow", "wrist", "hip", "knee", "ankle")}
        # Arm. Order matters: each joint needs its parent's D first.
        swing(f"{side}_shoulder", "chest", P[j["shoulder"]], P[j["elbow"]], lm(shoulder), lm(elbow))
        swing(f"{side}_elbow", f"{side}_shoulder", P[j["elbow"]], P[j["wrist"]], lm(elbow), lm(wrist))
        # Hand: wrist -> midpoint of the index and pinky knuckles. These small
        # landmarks are noisy, so the hand follows the forearm when unsure.
        knuckles = 0.5 * (lm(index) + lm(pinky))
        swing(f"{side}_wrist", f"{side}_elbow", P[j["wrist"]], skeleton.ends[j["wrist"]], lm(wrist), knuckles,
              trust=_confidence(visibility, index, pinky))  # fmt: skip
        # Leg.
        swing(f"{side}_hip", "hips", P[j["hip"]], P[j["knee"]], lm(hip), lm(knee))
        swing(f"{side}_knee", f"{side}_hip", P[j["knee"]], P[j["ankle"]], lm(knee), lm(ankle))
        # Foot: ankle -> toe tip.
        swing(f"{side}_ankle", f"{side}_knee", P[j["ankle"]], skeleton.ends[j["ankle"]], lm(ankle), lm(foot),
              trust=_confidence(visibility, foot))  # fmt: skip
    return D


# --------------------------------------------------------------------------
# World delta rotations -> what the file formats store
# --------------------------------------------------------------------------


def local_rotations(world_delta: np.ndarray, skeleton: Skeleton, identity_bind: bool = False) -> np.ndarray:
    """Convert world delta rotations D into parent-relative joint rotations.

    A joint's world rotation is W[j] = D[j] * B[j], where B[j] is its world
    rotation in the bind pose. An animation stores each joint relative to its
    parent, so local[j] = W[parent]^-1 * W[j].

    For the canonical skeleton B is the identity everywhere and this reduces to
    local[j] = D[parent]^-1 * D[j]: the identity in the bind pose. A character
    exported from Blender has a non-identity B per bone, and the same formula
    then reproduces that character's own rest rotations in the bind pose.

    identity_bind=True ignores B. BVH wants that: a BVH rest pose is defined by
    offsets alone, with every joint's axes aligned to the world.
    """
    T, J = world_delta.shape[:2]
    identity = np.array([0.0, 0.0, 0.0, 1.0])
    bind = np.tile(identity, (J, 1)) if identity_bind else skeleton.bind_rot
    world = quat_mul(world_delta, bind[None, :, :])

    root_parent = identity
    if not identity_bind:
        basis = skeleton.root_parent_matrix[:3, :3]
        root_parent = quat_from_matrix(basis / np.linalg.norm(basis, axis=0))

    local = np.zeros_like(world)
    for j, parent in enumerate(skeleton.parents):
        parent_world = np.broadcast_to(root_parent, (T, 4)) if parent < 0 else world[:, parent]
        local[:, j] = quat_mul(quat_conj(parent_world), world[:, j])
    return make_continuous(local)


def make_continuous(quats: np.ndarray) -> np.ndarray:
    """Remove sign flips along time (axis 0).

    q and -q are the same rotation, but a player that interpolates linearly
    between keyframes q and -q passes through zero and the joint whips round
    the long way. Flipping a keyframe whenever it points away from the previous
    one (negative dot product) keeps every step on the short path.
    """
    out = quats.copy()
    for t in range(1, len(out)):
        flip = np.sum(out[t] * out[t - 1], axis=-1) < 0
        out[t, flip] *= -1.0
    return out


def forward_kinematics(
    skeleton: Skeleton, world_delta: np.ndarray, root_pos: np.ndarray
) -> tuple[np.ndarray, dict[int, np.ndarray]]:
    """Joint positions implied by a solved motion.

    Returns (positions, tips): positions is (T, J, 3); tips maps each leaf
    joint to the (T, 3) position of its bone tip.

    Because D is a world-space delta, a bone's direction at time t is just its
    bind-pose offset rotated by its parent's D. Bone lengths never change.
    """
    T, J = world_delta.shape[:2]
    P = skeleton.bind_pos
    positions = np.zeros((T, J, 3))
    for j, parent in enumerate(skeleton.parents):  # parents always precede children
        if parent < 0:
            positions[:, j] = root_pos
        else:
            positions[:, j] = positions[:, parent] + quat_rotate(world_delta[:, parent], P[j] - P[parent])
    tips = {j: positions[:, j] + quat_rotate(world_delta[:, j], tip - P[j]) for j, tip in skeleton.ends.items()}
    return positions, tips


# --------------------------------------------------------------------------
# Root motion
# --------------------------------------------------------------------------


def _median_filter(values: np.ndarray, window: int) -> np.ndarray:
    half = window // 2
    padded = np.pad(values, half, mode="edge")
    return np.median(np.lib.stride_tricks.sliding_window_view(padded, window), axis=-1)


# Body segments used to measure how many metres one pixel covers.
_SCALE_SEGMENTS = (
    (L_SHOULDER, R_SHOULDER), (L_HIP, R_HIP), (L_SHOULDER, L_HIP), (R_SHOULDER, R_HIP),
    (L_HIP, L_KNEE), (R_HIP, R_KNEE), (L_KNEE, L_ANKLE), (R_KNEE, R_ANKLE),
    (L_SHOULDER, L_ELBOW), (R_SHOULDER, R_ELBOW), (L_ELBOW, L_WRIST), (R_ELBOW, R_WRIST),
)  # fmt: skip

# Landmarks that can touch the floor: heels and toe tips.
_FOOT_POINTS = (L_HEEL, R_HEEL, L_FOOT, R_FOOT)


def performer_leg_length(points: np.ndarray) -> float:
    """Median hip-knee-ankle length of the performer, in metres."""

    def leg(hip: int, knee: int, ankle: int) -> np.ndarray:
        return np.linalg.norm(points[:, knee] - points[:, hip], axis=-1) + np.linalg.norm(
            points[:, ankle] - points[:, knee], axis=-1
        )

    return float(np.median(0.5 * (leg(L_HIP, L_KNEE, L_ANKLE) + leg(R_HIP, R_KNEE, R_ANKLE))))


def foot_contacts(skeleton: Skeleton, world_delta: np.ndarray, joints: np.ndarray) -> np.ndarray:
    """Height of the lowest foot point of the posed character, per frame.

    Each foot has two points, matching the two foot landmarks the image gives
    us: the heel (in the bind pose, the point on the floor under the ankle)
    and the toe tip (the end of the foot bone). Both turn with the ankle joint.
    """
    P = skeleton.bind_pos
    lowest = np.full(len(joints), np.inf)
    for side in ("left", "right"):
        ankle = skeleton.index(f"{side}_ankle")
        heel = np.array([0.0, -P[ankle][1], 0.0])
        toe = skeleton.ends[ankle] - P[ankle]
        for offset in (heel, toe):
            point = joints[:, ankle] + quat_rotate(world_delta[:, ankle], offset)
            lowest = np.minimum(lowest, point[:, 1])
    return lowest


def solve_root_motion(track: PoseTrack, world_delta: np.ndarray, skeleton: Skeleton) -> np.ndarray:
    """World position of the root (hips) joint per frame, shape (T, 3).

    MediaPipe's world landmarks are always centred on the hips, so they say
    nothing about where the person is. The image landmarks do. Two things are
    read from the image:

      sideways travel   where the hips are, left to right
      height off floor  how far the lowest foot is above the floor line

    Everything else about the root's height comes from the character itself:
    the root is placed so that the character's own lowest foot point is exactly
    as high off the floor as the performer's. Feet that are on the ground
    in the video are therefore on the ground on the character, whatever its
    proportions, and a squat lowers the hips simply because the legs fold.

    Not recovered: travel towards or away from the camera. That needs the
    camera's focal length; depth is left at zero. The scale is also assumed
    roughly constant, i.e. a static camera and a performer who stays at about
    the same distance.
    """
    frames = track.frame_count

    # 1. Metres per pixel. The body is the ruler: the same segments are known
    #    in metres (world landmarks) and in pixels (image landmarks). World x/y
    #    lie in the image plane, so comparing in-plane lengths cancels
    #    foreshortening. The per-frame ratio wobbles with the pose by about
    #    10 percent, so it is median-filtered over a 3 second window: stable
    #    against that wobble, still able to follow a slow change in distance.
    metres = sum(np.linalg.norm(track.world[:, a, :2] - track.world[:, b, :2], axis=-1) for a, b in _SCALE_SEGMENTS)
    pixels = sum(np.linalg.norm(track.image[:, a] - track.image[:, b], axis=-1) for a, b in _SCALE_SEGMENTS)
    window = int(3 * track.fps) | 1  # odd
    scale = _median_filter(metres / np.maximum(pixels, EPS), window)
    # Performer metres -> character metres, so a character with shorter legs
    # travels proportionally less.
    scale = scale * skeleton.leg_length() / performer_leg_length(to_gltf_space(track.world))

    # 2. Sideways: hip centre relative to the middle of the image.
    hip_x = 0.5 * (track.image[:, L_HIP, 0] + track.image[:, R_HIP, 0])
    root = np.zeros((frames, 3))
    root[:, 0] = (hip_x - track.width / 2.0) * scale

    # 3. Height of the performer's lowest foot above the floor. Image y grows
    #    downward, so the lowest foot point has the LARGEST y. The floor line
    #    is where that point sits while standing, taken as a high percentile
    #    over the clip so a few noisy frames cannot move it.
    lowest_foot = np.max(track.image[:, _FOOT_POINTS, 1], axis=-1)
    floor = np.percentile(lowest_foot, 90)
    airborne = np.maximum(0.0, floor - lowest_foot) * scale

    # 4. Pose the character with the root at height zero, see how far below
    #    the root its lowest foot point hangs, and lift the root by that plus
    #    the airborne height.
    joints, _ = forward_kinematics(skeleton, world_delta, root)
    root[:, 1] = airborne - foot_contacts(skeleton, world_delta, joints)
    return root


# --------------------------------------------------------------------------
# Entry point
# --------------------------------------------------------------------------


@dataclass
class Motion:
    times: np.ndarray  # (T,) seconds
    world_delta: np.ndarray  # (T, J, 4), see solve_rotations
    root_pos: np.ndarray  # (T, 3) world position of the root joint
    camera_pitch_degrees: float = 0.0  # tilt that was removed, for the record


def solve(track: PoseTrack, skeleton: Skeleton) -> Motion:
    """Solve a gap-filled, smoothed pose track for the given skeleton."""
    points = to_gltf_space(track.world)
    pitch = estimate_camera_pitch(points)
    world_delta = solve_rotations(level(points, pitch), track.visibility, skeleton)
    root_pos = solve_root_motion(track, world_delta, skeleton)
    return Motion(
        times=np.arange(track.frame_count) / track.fps,
        world_delta=world_delta,
        root_pos=root_pos,
        camera_pitch_degrees=float(np.degrees(pitch)),
    )
