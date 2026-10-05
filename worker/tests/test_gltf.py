"""The files the worker writes: motion.glb (skeleton-only and with the default
character) and motion.bvh. Each is read back and evaluated independently of
the writer, then compared with forward kinematics on the solved motion.
"""

from __future__ import annotations

import json
import re
import shutil
import subprocess
from pathlib import Path

import numpy as np
import pytest

from rigforge_worker import bvh, gltf
from rigforge_worker import retarget as rt
from rigforge_worker.skeleton import from_gltf

import gltf_eval
from synth import RIG_DIR, axis_angle, canonical, performer, pose

REPO = Path(__file__).resolve().parents[2]
VALIDATOR = REPO / "web" / "scripts" / "validate-gltf.mjs"


def sample_motion(skeleton, frames=12):
    """A short motion with several joints moving and the root travelling.
    The performer is always the canonical skeleton; `skeleton` is the
    character the motion is solved for."""
    plain = canonical()
    clips = []
    for a in np.linspace(0, 80, frames):
        clips.append(
            pose(
                plain,
                {
                    "hips": axis_angle([0, 1, 0], a / 4),
                    "left_shoulder": axis_angle([0, 0, 1], -a),
                    "left_elbow": axis_angle([0, 1, 0], -a),
                    "right_hip": axis_angle([1, 0, 0], -a / 2),
                    "right_knee": axis_angle([1, 0, 0], a),
                },
            )
        )
    truth = np.concatenate(clips)
    root = np.tile(plain.bind_pos[0], (frames, 1))
    root[:, 0] += np.linspace(0, 0.5, frames)
    return rt.solve(performer(plain, truth, root), skeleton)


def default_character() -> gltf.Glb:
    return gltf.read_glb((RIG_DIR / "default_character.glb").read_bytes())


# ---- skeleton-only output ------------------------------------------------


def test_skeleton_only_glb_structure():
    skeleton = canonical()
    motion = sample_motion(skeleton)
    glb = gltf.read_glb(gltf.build_motion_glb(skeleton, motion, None, {"gaps": []}))
    doc = glb.doc

    assert doc["asset"]["version"] == "2.0"
    assert [n["name"] for n in doc["nodes"]] == skeleton.names
    assert doc["scenes"][0]["nodes"] == [0]
    assert doc["skins"][0]["joints"] == list(range(17))
    assert "meshes" not in doc

    [animation] = doc["animations"]
    paths = [(c["target"]["node"], c["target"]["path"]) for c in animation["channels"]]
    # One rotation channel per joint, plus the hips translation.
    assert paths == [(j, "rotation") for j in range(17)] + [(0, "translation")]
    assert animation["extras"] == {"gaps": []}

    times = gltf_eval.accessor(glb, animation["samplers"][0]["input"])[:, 0]
    assert np.allclose(times, motion.times)
    time_accessor = doc["accessors"][animation["samplers"][0]["input"]]
    assert time_accessor["min"] == [0.0] and time_accessor["max"] == [pytest.approx(motion.times[-1])]

    for sampler in animation["samplers"][:17]:
        rotations = gltf_eval.accessor(glb, sampler["output"])
        assert rotations.shape == (len(motion.times), 4)
        assert np.allclose(np.linalg.norm(rotations, axis=-1), 1.0, atol=1e-6), "keyframes must be unit quaternions"
    assert doc["buffers"][0]["byteLength"] == len(glb.bin)


def test_skeleton_only_glb_rest_pose_and_inverse_bind_matrices():
    skeleton = canonical()
    glb = gltf.read_glb(gltf.build_motion_glb(skeleton, sample_motion(skeleton), None))

    # The nodes' rest transforms reproduce the bind pose...
    assert np.allclose(gltf_eval.joint_positions(glb, skeleton.names), skeleton.bind_pos, atol=1e-6)
    # ...and each inverse bind matrix undoes its joint's bind world matrix.
    inverse_bind = gltf_eval.accessor(glb, glb.doc["skins"][0]["inverseBindMatrices"])
    worlds = gltf_eval.world_matrices(glb)
    for j in range(17):
        ibm = inverse_bind[j].reshape(4, 4).T  # column-major in the file
        assert np.allclose(worlds[j] @ ibm, np.eye(4), atol=1e-6)


def test_skeleton_only_glb_plays_back_the_solved_motion():
    skeleton = canonical()
    motion = sample_motion(skeleton)
    glb = gltf.read_glb(gltf.build_motion_glb(skeleton, motion, None))
    expected, _ = rt.forward_kinematics(skeleton, motion.world_delta, motion.root_pos)

    for frame in range(len(motion.times)):
        assert np.allclose(gltf_eval.joint_positions(glb, skeleton.names, frame), expected[frame], atol=1e-5)


# ---- output with the default character -----------------------------------


def test_default_character_matches_the_rigforge_skeleton():
    plain = canonical()
    character = default_character()
    skeleton = from_gltf(character.doc, plain)

    # Blender's export keeps every joint exactly where the skeleton JSON puts it.
    assert np.allclose(skeleton.bind_pos, plain.bind_pos, atol=1e-5)
    # Its bones do have their own axes, which is the case the bind-rotation
    # handling in retarget.local_rotations exists for.
    assert not np.allclose(np.abs(skeleton.bind_rot[:, 3]), 1.0, atol=1e-3)
    # One skin with exactly the 17 joints, and a mesh bound to it.
    [skin] = character.doc["skins"]
    assert sorted(character.doc["nodes"][i]["name"] for i in skin["joints"]) == sorted(plain.names)
    assert any("mesh" in n and n.get("skin") == 0 for n in character.doc["nodes"])


def test_character_glb_keeps_the_mesh_and_plays_back_the_solved_motion():
    character = default_character()
    skeleton = from_gltf(character.doc, canonical())
    motion = sample_motion(skeleton)

    glb = gltf.read_glb(gltf.build_motion_glb(skeleton, motion, character))

    # The character's mesh, skin and materials are carried over untouched.
    for key in ("meshes", "skins", "materials", "nodes", "scenes"):
        assert glb.doc[key] == character.doc[key], f"{key} changed"
    assert bytes(glb.bin[: len(character.bin)]) == bytes(character.bin), "existing buffer data must not move"
    assert len(glb.doc["animations"]) == 1

    # Evaluated like a viewer would, the animated joints land where forward
    # kinematics says they should, on a rig whose bones have their own axes.
    expected, _ = rt.forward_kinematics(skeleton, motion.world_delta, motion.root_pos)
    for frame in range(len(motion.times)):
        assert np.allclose(gltf_eval.joint_positions(glb, skeleton.names, frame), expected[frame], atol=1e-4)

    # The source character object is not modified by building the asset.
    assert "animations" not in character.doc


def test_t_pose_leaves_the_character_in_its_rest_pose():
    character = default_character()
    skeleton = from_gltf(character.doc, canonical())
    plain = canonical()
    motion = rt.solve(performer(plain, pose(plain, frames=2)), skeleton)

    glb = gltf.read_glb(gltf.build_motion_glb(skeleton, motion, character))

    rest = gltf_eval.joint_positions(glb, skeleton.names)
    animated = gltf_eval.joint_positions(glb, skeleton.names, frame=0)
    assert np.allclose(animated, rest, atol=1e-4)


def test_read_glb_rejects_garbage():
    from rigforge_worker.errors import PermanentError

    with pytest.raises(PermanentError):
        gltf.read_glb(b"definitely not a glb file at all")


# ---- Khronos validator ---------------------------------------------------


@pytest.mark.parametrize("with_character", [False, True], ids=["skeleton-only", "default-character"])
def test_output_passes_the_khronos_validator(tmp_path, with_character):
    if shutil.which("node") is None or not (VALIDATOR.parent.parent / "node_modules" / "gltf-validator").exists():
        from conftest import unavailable

        unavailable("glTF validator not installed; run `pnpm install` in web/")
    character = default_character() if with_character else None
    skeleton = from_gltf(character.doc, canonical()) if character else canonical()
    path = tmp_path / "motion.glb"
    path.write_bytes(gltf.build_motion_glb(skeleton, sample_motion(skeleton), character))

    result = subprocess.run(["node", str(VALIDATOR), "--json", str(path)], capture_output=True, text=True)
    report = json.loads(result.stdout)

    assert report["issues"]["numErrors"] == 0, report["issues"]["messages"]
    assert report["issues"]["numWarnings"] == 0, report["issues"]["messages"]
    assert result.returncode == 0
    assert report["info"]["animationCount"] == 1


# ---- BVH -----------------------------------------------------------------


def parse_bvh(text: str):
    """Parse a BVH file into (names, parents, offsets, channel_counts, frames, frame_time)."""
    header, motion = text.split("MOTION")
    names, parents, offsets, channels, stack = [], [], [], [], []
    in_end_site = False
    for line in header.splitlines():
        tokens = line.split()
        if not tokens:
            continue
        if tokens[0] in ("ROOT", "JOINT"):
            names.append(tokens[1])
            parents.append(stack[-1] if stack else -1)
            stack.append(len(names) - 1)
        elif tokens[0] == "End":
            in_end_site = True
        elif tokens[0] == "OFFSET" and not in_end_site:
            offsets.append([float(v) for v in tokens[1:]])
        elif tokens[0] == "CHANNELS":
            channels.append(tokens[2:])
        elif tokens[0] == "}":
            if in_end_site:
                in_end_site = False
            else:
                stack.pop()
    lines = motion.strip().splitlines()
    count = int(re.match(r"Frames:\s+(\d+)", lines[0]).group(1))
    frame_time = float(lines[1].split(":")[1])
    frames = np.array([[float(v) for v in line.split()] for line in lines[2:]])
    assert len(frames) == count
    return names, parents, np.array(offsets), channels, frames, frame_time


def rotation(axis: str, degrees: float) -> np.ndarray:
    c, s = np.cos(np.radians(degrees)), np.sin(np.radians(degrees))
    return {
        "X": np.array([[1, 0, 0], [0, c, -s], [0, s, c]]),
        "Y": np.array([[c, 0, s], [0, 1, 0], [-s, 0, c]]),
        "Z": np.array([[c, -s, 0], [s, c, 0], [0, 0, 1]]),
    }[axis]


def evaluate_bvh(names, parents, offsets, channels, values) -> np.ndarray:
    """World joint positions for one frame, following the BVH convention:
    rotations are applied in the order the channels are listed."""
    world_rot, world_pos = [None] * len(names), [None] * len(names)
    cursor = 0
    for j in range(len(names)):  # the file lists parents before children
        local_rot, translation = np.eye(3), offsets[j].copy()
        for channel in channels[j]:
            value = values[cursor]
            cursor += 1
            if channel.endswith("position"):
                translation["XYZ".index(channel[0])] = value
            else:
                local_rot = local_rot @ rotation(channel[0], value)
        if parents[j] < 0:
            world_rot[j], world_pos[j] = local_rot, translation
        else:
            world_rot[j] = world_rot[parents[j]] @ local_rot
            world_pos[j] = world_pos[parents[j]] + world_rot[parents[j]] @ translation
    return np.array(world_pos)


@pytest.mark.parametrize("with_character", [False, True], ids=["canonical", "default-character"])
def test_bvh_plays_back_the_solved_motion(with_character):
    skeleton = from_gltf(default_character().doc, canonical()) if with_character else canonical()
    motion = sample_motion(skeleton)

    names, parents, offsets, channels, frames, frame_time = parse_bvh(bvh.write_bvh(skeleton, motion))

    assert sorted(names) == sorted(skeleton.names)
    assert len(channels[0]) == 6 and all(len(c) == 3 for c in channels[1:])
    assert frames.shape == (len(motion.times), 6 + 3 * 16)
    assert frame_time == pytest.approx(1 / 30, abs=1e-6)

    expected, _ = rt.forward_kinematics(skeleton, motion.world_delta, motion.root_pos)
    order = [skeleton.index(name) for name in names]
    for frame in range(len(frames)):
        positions = evaluate_bvh(names, parents, offsets, channels, frames[frame])
        # BVH is written in centimetres.
        assert np.allclose(positions / 100.0, expected[frame][order], atol=1e-4)


def test_bvh_has_an_end_site_for_every_leaf():
    text = bvh.write_bvh(canonical(), sample_motion(canonical(), frames=2))
    assert text.count("End Site") == 5  # head, two wrists, two ankles
    assert text.startswith("HIERARCHY\nROOT hips\n")
