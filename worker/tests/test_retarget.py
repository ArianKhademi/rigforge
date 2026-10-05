"""Unit tests for the retargeting maths on synthetic landmark sets."""

from __future__ import annotations

import numpy as np
import pytest

from rigforge_worker import retarget as rt
from rigforge_worker.skeleton import Skeleton

from synth import IDENTITY, PIXELS_PER_METRE, angle_degrees, axis_angle, canonical, performer, pose

TOLERANCE_DEGREES = 0.5


def solve_local(track, skeleton):
    motion = rt.solve(track, skeleton)
    return motion, rt.local_rotations(motion.world_delta, skeleton)


# ---- quaternion helpers --------------------------------------------------


def random_quats(n, seed=0):
    q = np.random.default_rng(seed).normal(size=(n, 4))
    return q / np.linalg.norm(q, axis=-1, keepdims=True)


def test_matrix_round_trip():
    q = random_quats(200)
    back = rt.quat_from_matrix(rt.quat_to_matrix(q))
    # q and -q are the same rotation.
    assert np.allclose(np.abs(np.sum(q * back, axis=-1)), 1.0, atol=1e-9)


def test_quat_mul_matches_matrix_product():
    a, b = random_quats(50, 1), random_quats(50, 2)
    assert np.allclose(rt.quat_to_matrix(rt.quat_mul(a, b)), rt.quat_to_matrix(a) @ rt.quat_to_matrix(b), atol=1e-9)


def test_quat_rotate_matches_matrix():
    q, v = random_quats(50, 3), np.random.default_rng(4).normal(size=(50, 3))
    assert np.allclose(rt.quat_rotate(q, v), np.einsum("nij,nj->ni", rt.quat_to_matrix(q), v), atol=1e-9)


def test_quat_between_is_the_shortest_arc():
    rng = np.random.default_rng(5)
    a, b = rt.normalize(rng.normal(size=(100, 3))), rt.normalize(rng.normal(size=(100, 3)))
    q = rt.quat_between(a, b)
    assert np.allclose(rt.quat_rotate(q, a), b, atol=1e-9)
    # Shortest arc: the rotation angle equals the angle between the vectors.
    between = np.degrees(np.arccos(np.clip(np.sum(a * b, axis=-1), -1, 1)))
    assert np.allclose(angle_degrees(q), between, atol=1e-6)


def test_quat_between_opposite_vectors_is_a_half_turn():
    for a in (np.array([1.0, 0, 0]), np.array([0, 1.0, 0]), np.array([0, 0, -1.0])):
        q = rt.quat_between(a, -a)
        assert np.isclose(np.linalg.norm(q), 1.0)
        assert np.allclose(rt.quat_rotate(q, a), -a, atol=1e-9)


def test_slerp_endpoints_and_midpoint():
    a, b = IDENTITY, axis_angle([0, 1, 0], 90)
    assert np.allclose(rt.quat_slerp(a, b, 0.0), a)
    assert np.allclose(rt.quat_slerp(a, b, 1.0), b)
    assert np.allclose(rt.quat_slerp(a, b, 0.5), axis_angle([0, 1, 0], 45))


# ---- the two cases the spec names ----------------------------------------


def test_t_pose_yields_identity_local_rotations():
    skeleton = canonical()
    track = performer(skeleton, pose(skeleton, frames=3))

    motion, local = solve_local(track, skeleton)

    for j, name in enumerate(skeleton.names):
        assert angle_degrees(local[:, j]).max() < 1e-4, f"{name} is rotated in a T-pose"
    # And the character stands where the bind pose stands: hips at bind height.
    assert np.allclose(motion.root_pos, skeleton.bind_pos[skeleton.index("hips")], atol=1e-6)


def test_known_elbow_bend_yields_expected_angle():
    skeleton = canonical()
    # Bend the left elbow 90 degrees so the forearm points forward (+Z). In
    # the bind pose the forearm points along +X; turning +X onto +Z is a
    # rotation of -90 degrees about +Y.
    bend = axis_angle([0, 1, 0], -90)
    track = performer(skeleton, pose(skeleton, {"left_elbow": bend}))

    # Sanity-check the test input itself: the forearm really points forward.
    points = rt.to_gltf_space(track.world)[0]
    forearm = rt.normalize(points[rt.L_WRIST] - points[rt.L_ELBOW])
    assert np.allclose(forearm, [0, 0, 1], atol=1e-9)

    _, local = solve_local(track, skeleton)

    elbow = local[0, skeleton.index("left_elbow")]
    assert angle_degrees(elbow) == pytest.approx(90.0, abs=TOLERANCE_DEGREES)
    axis = elbow[:3] / np.linalg.norm(elbow[:3]) * np.sign(elbow[3])
    assert np.allclose(axis, [0, -1, 0], atol=0.01), "the bend must be about the vertical axis"
    # Only the elbow moved.
    for j, name in enumerate(skeleton.names):
        if name != "left_elbow":
            assert angle_degrees(local[0, j]) < TOLERANCE_DEGREES, f"{name} should not rotate"


@pytest.mark.parametrize(
    "joint, axis, degrees",
    [
        ("right_knee", [1, 0, 0], 60),  # knee flexion
        ("left_shoulder", [0, 0, 1], -70),  # arm lowered to the side
        ("right_hip", [1, 0, 0], -45),  # leg raised forward
        ("right_elbow", [0, 1, 0], 120),
    ],
)
def test_single_joint_bends_are_recovered(joint, axis, degrees):
    skeleton = canonical()
    track = performer(skeleton, pose(skeleton, {joint: axis_angle(axis, degrees)}))

    _, local = solve_local(track, skeleton)

    assert angle_degrees(local[0, skeleton.index(joint)]) == pytest.approx(abs(degrees), abs=TOLERANCE_DEGREES)
    others = [angle_degrees(local[0, j]) for j, name in enumerate(skeleton.names) if name != joint]
    assert max(others) < TOLERANCE_DEGREES


# ---- properties ----------------------------------------------------------


def test_turning_the_whole_body_rotates_only_the_root():
    skeleton = canonical()
    turn = axis_angle([0, 1, 0], 90)  # the performer turns to face +X
    track = performer(skeleton, pose(skeleton, {"hips": turn}))

    _, local = solve_local(track, skeleton)

    hips = skeleton.index("hips")
    assert np.allclose(np.abs(np.sum(local[0, hips] * turn)), 1.0, atol=1e-6)
    assert max(angle_degrees(local[0, j]) for j in range(len(skeleton.names)) if j != hips) < TOLERANCE_DEGREES


def test_solved_pose_reproduces_every_observed_bone_direction():
    """A pose with many joints bent at once: the solver cannot know twist, but
    forward kinematics on its answer must put every joint where it was seen."""
    skeleton = canonical()
    rng = np.random.default_rng(7)
    local = {"hips": axis_angle([0, 1, 0], 35)}
    for name in skeleton.names:
        if any(part in name for part in ("shoulder", "elbow", "hip", "knee")):
            local[name] = axis_angle(rng.normal(size=3), rng.uniform(10, 80))
    truth = pose(skeleton, local)
    root = np.array([[0.2, 1.1, 0.0]])
    expected, _ = rt.forward_kinematics(skeleton, truth, root)

    # solve_rotations directly: this single frame has no upright posture for
    # the camera-tilt calibration in solve() to work from.
    track = performer(skeleton, truth, root)
    solved_delta = rt.solve_rotations(rt.to_gltf_space(track.world), track.visibility, skeleton)
    solved, _ = rt.forward_kinematics(skeleton, solved_delta, root)

    assert np.allclose(solved, expected, atol=1e-6)


def test_rotations_do_not_depend_on_the_performers_proportions():
    skeleton = canonical()
    # A performer 30% bigger with arms that are longer still.
    big = canonical()
    big.bind_pos = big.bind_pos * 1.3
    big.ends = {j: tip * 1.3 for j, tip in big.ends.items()}
    for name in ("left_elbow", "left_wrist", "right_elbow", "right_wrist"):
        j = big.index(name)
        big.bind_pos[j, 0] *= 1.25
        if j in big.ends:
            big.ends[j][0] *= 1.25

    bends = {"left_elbow": axis_angle([0, 1, 0], -90), "right_knee": axis_angle([1, 0, 0], 50)}
    _, from_big = solve_local(performer(big, pose(big, bends)), skeleton)
    _, from_same = solve_local(performer(skeleton, pose(skeleton, bends)), skeleton)

    assert np.allclose(np.abs(np.sum(from_big * from_same, axis=-1)), 1.0, atol=1e-6)


def test_character_with_its_own_bone_axes_keeps_its_rest_pose_in_a_t_pose():
    """Characters exported from Blender have a rest rotation on every bone.
    In a T-pose the animation must reproduce exactly those rest rotations."""
    skeleton = with_random_bone_axes(canonical())
    rest_local = glTF_rest_rotations(skeleton)

    # The performer is the plain canonical skeleton in a T-pose.
    _, local = solve_local(performer(canonical(), pose(canonical())), skeleton)

    assert np.allclose(np.abs(np.sum(local[0] * rest_local, axis=-1)), 1.0, atol=1e-6)


def test_character_with_its_own_bone_axes_is_posed_correctly():
    """Evaluate the local rotations the way a glTF player does (compose rest
    translations and animated rotations down the hierarchy) and compare with
    where the performer's joints were."""
    plain = canonical()
    skeleton = with_random_bone_axes(canonical())
    bends = {
        "hips": axis_angle([0, 1, 0], 20),
        "left_shoulder": axis_angle([0, 0, 1], -60),
        "left_elbow": axis_angle([0, 1, 0], -80),
        "right_hip": axis_angle([1, 0, 0], -50),
        "right_knee": axis_angle([1, 0, 0], 70),
    }
    truth = pose(plain, bends)
    root = np.tile(plain.bind_pos[0], (1, 1))
    expected, _ = rt.forward_kinematics(plain, truth, root)

    track = performer(plain, truth, root)
    solved_delta = rt.solve_rotations(rt.to_gltf_space(track.world), track.visibility, skeleton)
    local = rt.local_rotations(solved_delta, skeleton)

    positions = evaluate_like_gltf(skeleton, local[0], root[0])
    assert np.allclose(positions, expected[0], atol=1e-6)


def with_random_bone_axes(skeleton: Skeleton) -> Skeleton:
    rng = np.random.default_rng(11)
    q = rng.normal(size=(len(skeleton.names), 4))
    skeleton.bind_rot = q / np.linalg.norm(q, axis=-1, keepdims=True)
    return skeleton


def glTF_rest_rotations(skeleton: Skeleton) -> np.ndarray:
    """Each joint's bind rotation relative to its parent: what the character
    file stores as the node's rest rotation."""
    rest = np.zeros_like(skeleton.bind_rot)
    for j, parent in enumerate(skeleton.parents):
        rest[j] = (
            skeleton.bind_rot[j]
            if parent < 0
            else rt.quat_mul(rt.quat_conj(skeleton.bind_rot[parent]), skeleton.bind_rot[j])
        )
    return rest


def evaluate_like_gltf(skeleton: Skeleton, local: np.ndarray, root_pos: np.ndarray) -> np.ndarray:
    """World joint positions from local rotations plus rest translations."""
    world_rot = np.zeros_like(local)
    world_pos = np.zeros((len(local), 3))
    for j, parent in enumerate(skeleton.parents):
        if parent < 0:
            world_rot[j], world_pos[j] = local[j], root_pos
            continue
        # Rest translation: the bind offset expressed in the parent's bind axes.
        offset = skeleton.bind_pos[j] - skeleton.bind_pos[parent]
        rest_translation = rt.quat_rotate(rt.quat_conj(skeleton.bind_rot[parent]), offset)
        world_rot[j] = rt.quat_mul(world_rot[parent], local[j])
        world_pos[j] = world_pos[parent] + rt.quat_rotate(world_rot[parent], rest_translation)
    return world_pos


def test_keyframes_never_flip_sign():
    skeleton = canonical()
    # Swing the arm through a full turn: its quaternion crosses w = 0, which is
    # where a naive conversion flips sign.
    frames = 90
    truth = np.concatenate(
        [pose(skeleton, {"left_shoulder": axis_angle([0, 0, 1], a)}) for a in np.linspace(0, 360, frames)]
    )

    _, local = solve_local(performer(skeleton, truth), skeleton)

    dots = np.sum(local[1:] * local[:-1], axis=-1)
    assert dots.min() > 0.0, "consecutive keyframes must be on the same side of the quaternion sphere"


# ---- camera tilt and head calibration ------------------------------------


def test_camera_pitch_is_estimated_and_removed():
    skeleton = canonical()
    bends = {"left_elbow": axis_angle([0, 1, 0], -90)}
    track = performer(skeleton, pose(skeleton, bends, frames=5), camera_pitch_degrees=15.0)

    # Seen through a tilted camera the performer appears to lean forward.
    raw = rt.to_gltf_space(track.world)[0]
    spine = raw[rt.L_SHOULDER] + raw[rt.R_SHOULDER] - raw[rt.L_ANKLE] - raw[rt.R_ANKLE]
    assert np.degrees(np.arctan2(spine[2], spine[1])) == pytest.approx(15.0, abs=0.01)

    motion, local = solve_local(track, skeleton)

    assert motion.camera_pitch_degrees == pytest.approx(15.0, abs=0.01)
    # With the tilt removed the answer is the same as with a level camera.
    assert angle_degrees(local[0, skeleton.index("hips")]) < TOLERANCE_DEGREES
    assert angle_degrees(local[0, skeleton.index("left_elbow")]) == pytest.approx(90.0, abs=TOLERANCE_DEGREES)


def test_head_pitch_is_measured_relative_to_the_standing_posture():
    skeleton = canonical()
    # Frames 0-19: standing, looking ahead. Frames 20-29: the head nods 30
    # degrees down. "Neutral" is the median posture, so most of the clip must
    # be spent looking ahead for it to be found.
    nod = axis_angle([1, 0, 0], 30)
    truth = np.concatenate([pose(skeleton, frames=20), pose(skeleton, {"head": nod}, frames=10)])
    # MediaPipe-like bias: eyes sit 25 degrees above the ear line.
    track = performer(skeleton, truth, eye_bias_degrees=25.0)

    _, local = solve_local(track, skeleton)

    head, neck = skeleton.index("head"), skeleton.index("neck")
    # The bias does not show up as a permanently raised head...
    assert angle_degrees(local[0, head]) < TOLERANCE_DEGREES
    assert angle_degrees(local[0, neck]) < TOLERANCE_DEGREES
    # ...and the nod is shared between neck and head: 15 degrees each.
    assert angle_degrees(local[25, neck]) == pytest.approx(15.0, abs=TOLERANCE_DEGREES)
    assert angle_degrees(local[25, head]) == pytest.approx(15.0, abs=TOLERANCE_DEGREES)


def test_low_visibility_hands_follow_the_forearm():
    skeleton = canonical()
    track = performer(skeleton, pose(skeleton, {"left_wrist": axis_angle([0, 0, 1], 60)}))
    wrist = skeleton.index("left_wrist")

    _, seen = solve_local(track, skeleton)
    assert angle_degrees(seen[0, wrist]) == pytest.approx(60.0, abs=TOLERANCE_DEGREES)

    track.visibility[:, [rt.L_INDEX, rt.L_PINKY]] = 0.1  # knuckles not actually visible
    _, unseen = solve_local(track, skeleton)
    assert angle_degrees(unseen[0, wrist]) < TOLERANCE_DEGREES


# ---- root motion ---------------------------------------------------------


def test_sidestep_moves_the_root_sideways():
    skeleton = canonical()
    frames = 30
    root = np.tile(skeleton.bind_pos[0], (frames, 1))
    root[:, 0] += np.linspace(0.0, 0.8, frames)  # 80 cm to the performer's left

    motion = rt.solve(performer(skeleton, pose(skeleton, frames=frames), root), skeleton)

    assert np.allclose(motion.root_pos[:, 0], root[:, 0], atol=1e-3)
    assert np.allclose(motion.root_pos[:, 1], root[:, 1], atol=1e-3)  # height unchanged
    assert np.allclose(motion.root_pos[:, 2], 0.0)  # depth is not recovered


def test_jump_lifts_the_root_and_landing_returns_it():
    skeleton = canonical()
    frames = 60
    height = 0.4 * np.sin(np.linspace(0, np.pi, frames)) ** 2
    height[:15], height[-15:] = 0.0, 0.0  # on the ground before and after
    root = np.tile(skeleton.bind_pos[0], (frames, 1))
    root[:, 1] += height

    motion = rt.solve(performer(skeleton, pose(skeleton, frames=frames), root), skeleton)

    assert np.allclose(motion.root_pos[:, 1], root[:, 1], atol=1e-3)
    assert motion.root_pos[:, 1].max() == pytest.approx(skeleton.bind_pos[0, 1] + height.max(), abs=1e-3)


def test_squat_lowers_the_hips_and_keeps_the_feet_on_the_floor():
    skeleton = canonical()
    # Fold both legs: thighs forward 80 degrees, knees back 110 degrees.
    squat = {
        "left_hip": axis_angle([1, 0, 0], -80), "right_hip": axis_angle([1, 0, 0], -80),
        "left_knee": axis_angle([1, 0, 0], 110), "right_knee": axis_angle([1, 0, 0], 110),
    }  # fmt: skip
    # Half the clip standing, half squatting. The performer's root is placed
    # so the feet stay planted, as a real squat does.
    standing, squatting = pose(skeleton, frames=10), pose(skeleton, squat, frames=10)
    truth = np.concatenate([standing, squatting])
    root = np.zeros((20, 3))
    joints, _ = rt.forward_kinematics(skeleton, truth, root)
    root[:, 1] = -rt.foot_contacts(skeleton, truth, joints)

    motion = rt.solve(performer(skeleton, truth, root), skeleton)

    assert motion.root_pos[15, 1] < motion.root_pos[5, 1] - 0.25, "hips must drop in the squat"
    solved_joints, _ = rt.forward_kinematics(skeleton, motion.world_delta, motion.root_pos)
    lowest = rt.foot_contacts(skeleton, motion.world_delta, solved_joints)
    assert np.allclose(lowest, 0.0, atol=2e-3), "feet must stay on the floor"


def test_root_travel_scales_with_the_characters_leg_length():
    performer_skeleton = canonical()
    small = canonical()  # a character half the size
    small.bind_pos = small.bind_pos * 0.5
    small.ends = {j: tip * 0.5 for j, tip in small.ends.items()}
    frames = 20
    root = np.tile(performer_skeleton.bind_pos[0], (frames, 1))
    root[:, 0] += np.linspace(0.0, 1.0, frames)

    motion = rt.solve(performer(performer_skeleton, pose(performer_skeleton, frames=frames), root), small)

    travelled = motion.root_pos[-1, 0] - motion.root_pos[0, 0]
    assert travelled == pytest.approx(0.5, abs=1e-3), "a half-size character covers half the distance"
    assert motion.root_pos[0, 1] == pytest.approx(small.bind_pos[0, 1], abs=1e-3)


def test_forward_kinematics_preserves_bone_lengths():
    skeleton = canonical()
    q = random_quats(len(skeleton.names) * 4, seed=9).reshape(4, len(skeleton.names), 4)
    joints, _ = rt.forward_kinematics(skeleton, q, np.zeros((4, 3)))
    for j, parent in enumerate(skeleton.parents):
        if parent >= 0:
            bind = np.linalg.norm(skeleton.bind_pos[j] - skeleton.bind_pos[parent])
            assert np.allclose(np.linalg.norm(joints[:, j] - joints[:, parent], axis=-1), bind)


def test_scale_constant_matches_the_synthetic_camera():
    # Guards the synthetic camera itself: 1 metre must be PIXELS_PER_METRE pixels.
    skeleton = canonical()
    track = performer(skeleton, pose(skeleton))
    shoulder_width_px = track.image[0, rt.L_SHOULDER, 0] - track.image[0, rt.R_SHOULDER, 0]
    assert shoulder_width_px == pytest.approx(0.36 * PIXELS_PER_METRE)
