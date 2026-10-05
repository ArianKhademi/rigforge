"""A small glTF 2.0 binary (.glb) reader and writer.

Hand-written rather than using a library because the job is narrow: build a
skeleton, or open a character file, and attach one animation. Writing it
directly keeps every byte of the output explainable.

GLB layout: a 12-byte header, then chunks. Chunk 0 is the JSON document that
describes the scene; chunk 1 is one binary buffer holding all the numbers.
The JSON refers to that buffer in two steps: a bufferView is a byte range, and
an accessor says how to read a bufferView as typed data (e.g. "N VEC4 floats").
"""

from __future__ import annotations

import copy
import json
import struct
from dataclasses import dataclass

import numpy as np

from .errors import PermanentError
from .retarget import Motion, local_rotations
from .skeleton import Skeleton

GLB_MAGIC = 0x46546C67  # "glTF"
CHUNK_JSON = 0x4E4F534A  # "JSON"
CHUNK_BIN = 0x004E4942  # "BIN\0"
FLOAT = 5126  # glTF componentType for 32-bit float


@dataclass
class Glb:
    doc: dict
    bin: bytearray


def read_glb(data: bytes) -> Glb:
    if len(data) < 20 or struct.unpack_from("<I", data, 0)[0] != GLB_MAGIC:
        raise PermanentError("character file is not a binary glTF")
    doc, binary = None, bytearray()
    offset = 12
    while offset + 8 <= len(data):
        length, kind = struct.unpack_from("<II", data, offset)
        body = data[offset + 8 : offset + 8 + length]
        if kind == CHUNK_JSON:
            doc = json.loads(body)
        elif kind == CHUNK_BIN:
            binary = bytearray(body)
        offset += 8 + length
    if doc is None:
        raise PermanentError("character file has no JSON chunk")
    return Glb(doc=doc, bin=binary)


def write_glb(glb: Glb) -> bytes:
    def pad(buf: bytes, fill: bytes) -> bytes:
        # Every chunk must be a multiple of 4 bytes long.
        return buf + fill * (-len(buf) % 4)

    binary = pad(bytes(glb.bin), b"\x00")
    doc = copy.deepcopy(glb.doc)
    doc.setdefault("buffers", [{}])[0]["byteLength"] = len(binary)
    json_chunk = pad(json.dumps(doc, separators=(",", ":")).encode("utf-8"), b" ")

    total = 12 + 8 + len(json_chunk) + 8 + len(binary)
    return b"".join(
        [
            struct.pack("<III", GLB_MAGIC, 2, total),
            struct.pack("<II", len(json_chunk), CHUNK_JSON),
            json_chunk,
            struct.pack("<II", len(binary), CHUNK_BIN),
            binary,
        ]
    )


def _add_accessor(glb: Glb, values: np.ndarray, kind: str, with_bounds: bool = False) -> int:
    """Append float data to the binary buffer and describe it with a new
    bufferView and accessor. Returns the accessor index."""
    data = np.ascontiguousarray(values, dtype="<f4")
    glb.bin.extend(b"\x00" * (-len(glb.bin) % 4))  # floats must start 4-byte aligned
    views = glb.doc.setdefault("bufferViews", [])
    views.append({"buffer": 0, "byteOffset": len(glb.bin), "byteLength": data.nbytes})
    glb.bin.extend(data.tobytes())

    accessor = {"bufferView": len(views) - 1, "componentType": FLOAT, "count": len(data), "type": kind}
    if with_bounds:  # the spec requires min/max on animation time inputs
        flat = data.reshape(len(data), -1)
        accessor["min"] = [float(v) for v in flat.min(axis=0)]
        accessor["max"] = [float(v) for v in flat.max(axis=0)]
    accessors = glb.doc.setdefault("accessors", [])
    accessors.append(accessor)
    return len(accessors) - 1


def skeleton_document(skeleton: Skeleton) -> Glb:
    """A glTF scene containing only the skeleton: one node per joint and a skin."""
    glb = Glb(doc={"asset": {"version": "2.0", "generator": "rigforge"}}, bin=bytearray())
    nodes = []
    for j, name in enumerate(skeleton.names):
        parent = skeleton.parents[j]
        # A node's translation is relative to its parent. With identity rest
        # rotations that is just the difference of world positions.
        offset = skeleton.bind_pos[j] - (skeleton.bind_pos[parent] if parent >= 0 else 0.0)
        node = {"name": name, "translation": [float(v) for v in offset]}
        children = skeleton.children(j)
        if children:
            node["children"] = children
        nodes.append(node)
    root = skeleton.parents.index(-1)

    # An inverse bind matrix takes a vertex from model space into the joint's
    # own space in the bind pose. With no rest rotation it is a translation by
    # minus the joint's world position. glTF stores matrices column by column,
    # so the translation occupies elements 12..14.
    inverse_bind = np.tile(np.eye(4, dtype=np.float32).reshape(16), (len(nodes), 1))
    inverse_bind[:, 12:15] = -skeleton.bind_pos

    glb.doc.update(
        {
            "scene": 0,
            "scenes": [{"name": "Rigforge", "nodes": [root]}],
            "nodes": nodes,
            "skins": [
                {
                    "name": "rigforge",
                    "joints": list(range(len(nodes))),
                    "skeleton": root,
                    "inverseBindMatrices": _add_accessor(glb, inverse_bind, "MAT4"),
                }
            ],
            "buffers": [{"byteLength": 0}],
        }
    )
    return glb


def joint_nodes(doc: dict, skeleton: Skeleton) -> list[int]:
    """Node index of every skeleton joint, looked up by name."""
    by_name = {node.get("name"): i for i, node in enumerate(doc.get("nodes", []))}
    return [by_name[name] for name in skeleton.names]


def add_animation(glb: Glb, skeleton: Skeleton, motion: Motion, extras: dict | None = None) -> None:
    """Attach the motion to the document as its only animation.

    One channel per joint drives that joint's rotation, and one extra channel
    drives the root joint's translation. All channels share a single time
    accessor (one keyframe per video frame).
    """
    targets = joint_nodes(glb.doc, skeleton)
    rotations = local_rotations(motion.world_delta, skeleton)

    # Root positions are solved in world space; the root node's translation is
    # relative to its parent node, so move them into the parent's space.
    to_parent = np.linalg.inv(skeleton.root_parent_matrix)
    homogeneous = np.concatenate([motion.root_pos, np.ones((len(motion.root_pos), 1))], axis=1)
    root_translation = (homogeneous @ to_parent.T)[:, :3]

    time_accessor = _add_accessor(glb, motion.times.reshape(-1, 1), "SCALAR", with_bounds=True)
    samplers, channels = [], []

    def channel(node: int, path: str, values: np.ndarray, kind: str) -> None:
        samplers.append({"input": time_accessor, "output": _add_accessor(glb, values, kind), "interpolation": "LINEAR"})
        channels.append({"sampler": len(samplers) - 1, "target": {"node": node, "path": path}})

    for j, node in enumerate(targets):
        # Re-normalise after the cast to float32: the validator rejects
        # rotation keyframes that are not unit length.
        q = rotations[:, j].astype(np.float32)
        q /= np.linalg.norm(q, axis=-1, keepdims=True)
        channel(node, "rotation", q, "VEC4")
    channel(targets[skeleton.parents.index(-1)], "translation", root_translation, "VEC3")

    animation = {"name": "motion", "samplers": samplers, "channels": channels}
    if extras:
        animation["extras"] = extras
    glb.doc["animations"] = [animation]  # replaces any animation the character shipped with


def build_motion_glb(skeleton: Skeleton, motion: Motion, character: Glb | None, extras: dict | None = None) -> bytes:
    """The motion asset: the animated skeleton, with the character mesh
    skinned to it when a character was selected."""
    if character is None:
        glb = skeleton_document(skeleton)
    else:
        glb = Glb(doc=copy.deepcopy(character.doc), bin=bytearray(character.bin))
    add_animation(glb, skeleton, motion, extras)
    return write_glb(glb)
