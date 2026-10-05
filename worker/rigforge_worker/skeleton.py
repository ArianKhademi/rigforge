"""The skeleton a motion is solved for: joint names, hierarchy and bind pose.

It comes from one of two places: the canonical definition in
rig/rigforge_skeleton.json, or the joint nodes of a character GLB (same joint
names and hierarchy, but the character's own proportions and bone axes).
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path

import numpy as np

from .errors import PermanentError
from .retarget import quat_from_matrix, quat_to_matrix

IDENTITY_QUAT = np.array([0.0, 0.0, 0.0, 1.0])


@dataclass
class Skeleton:
    names: list[str]
    parents: list[int]  # index of each joint's parent, -1 for the root
    # World-space bind pose. For the canonical skeleton every rotation is the
    # identity; a character exported from Blender has a rotation per bone.
    bind_pos: np.ndarray  # (J, 3)
    bind_rot: np.ndarray  # (J, 4) quaternions, xyzw
    # World position of the bone tip for joints without a child joint (head,
    # wrists, ankles). Gives the leaf bones a direction to aim.
    ends: dict[int, np.ndarray] = field(default_factory=dict)
    # World matrix of the node the root joint hangs from (identity if none).
    # Exporters often wrap the skeleton in an "Armature" node with its own
    # transform; root motion has to be expressed inside it.
    root_parent_matrix: np.ndarray = field(default_factory=lambda: np.eye(4))

    def index(self, name: str) -> int:
        return self.names.index(name)

    def children(self, joint: int) -> list[int]:
        return [j for j, parent in enumerate(self.parents) if parent == joint]

    def leg_length(self) -> float:
        """Hip to knee plus knee to ankle, averaged over both legs. Used to
        scale the performer's root motion to the character's size."""
        total = 0.0
        for side in ("left", "right"):
            hip, knee, ankle = (self.bind_pos[self.index(f"{side}_{part}")] for part in ("hip", "knee", "ankle"))
            total += np.linalg.norm(knee - hip) + np.linalg.norm(ankle - knee)
        return total / 2.0


def load_canonical(path: Path) -> Skeleton:
    data = json.loads(path.read_text())
    joints = data["joints"]
    return Skeleton(
        names=[j["name"] for j in joints],
        parents=[j["parent"] for j in joints],
        bind_pos=np.array([j["position"] for j in joints], dtype=np.float64),
        bind_rot=np.tile(IDENTITY_QUAT, (len(joints), 1)),
        ends={i: np.array(j["end"], dtype=np.float64) for i, j in enumerate(joints) if "end" in j},
    )


def from_gltf(doc: dict, canonical: Skeleton) -> Skeleton:
    """Read a character's skeleton from its glTF JSON.

    The api already validated the upload (api/internal/character/rig.go); the
    checks here guard the worker against a character that changed since.
    """
    nodes = doc.get("nodes", [])
    by_name = {node.get("name"): i for i, node in enumerate(nodes)}
    missing = [name for name in canonical.names if name not in by_name]
    if missing:
        raise PermanentError(f"character is missing Rigforge joints: {', '.join(missing)}")

    parent_of = {child: i for i, node in enumerate(nodes) for child in node.get("children", [])}

    def local_matrix(node: dict) -> np.ndarray:
        if "matrix" in node:
            return np.array(node["matrix"], dtype=np.float64).reshape(4, 4).T  # glTF is column-major
        m = np.eye(4)
        rotation = np.array(node.get("rotation", [0, 0, 0, 1]), dtype=np.float64)
        scale = np.array(node.get("scale", [1, 1, 1]), dtype=np.float64)
        m[:3, :3] = quat_to_matrix(rotation) * scale  # scales the columns
        m[:3, 3] = node.get("translation", [0, 0, 0])
        return m

    cache: dict[int, np.ndarray] = {}

    def world_matrix(index: int) -> np.ndarray:
        if index not in cache:
            local = local_matrix(nodes[index])
            cache[index] = world_matrix(parent_of[index]) @ local if index in parent_of else local
        return cache[index]

    joint_nodes = [by_name[name] for name in canonical.names]
    bind_pos = np.zeros((len(joint_nodes), 3))
    bind_rot = np.zeros((len(joint_nodes), 4))
    for j, node_index in enumerate(joint_nodes):
        m = world_matrix(node_index)
        bind_pos[j] = m[:3, 3]
        # Strip scale from the upper 3x3 to get a pure rotation.
        basis = m[:3, :3] / np.linalg.norm(m[:3, :3], axis=0)
        bind_rot[j] = quat_from_matrix(basis)

    root_node = joint_nodes[canonical.parents.index(-1)]
    root_parent = world_matrix(parent_of[root_node]) if root_node in parent_of else np.eye(4)

    skeleton = Skeleton(
        names=list(canonical.names),
        parents=list(canonical.parents),
        bind_pos=bind_pos,
        bind_rot=bind_rot,
        root_parent_matrix=root_parent,
    )
    # A GLB stores no bone tips, so place leaf tips by scaling the canonical
    # leaf bones to this character's size (ratio of leg lengths).
    scale = skeleton.leg_length() / canonical.leg_length()
    for joint, tip in canonical.ends.items():
        skeleton.ends[joint] = bind_pos[joint] + (tip - canonical.bind_pos[joint]) * scale
    return skeleton
