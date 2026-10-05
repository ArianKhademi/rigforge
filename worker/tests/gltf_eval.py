"""A minimal glTF evaluator for tests: read accessors and compute where the
nodes of a document are at a given animation keyframe. It is written against
the glTF spec, independently of the writer in rigforge_worker.gltf, so the
tests check the files the way a third-party viewer would read them.
"""

from __future__ import annotations

import numpy as np

from rigforge_worker.gltf import Glb

COMPONENTS = {"SCALAR": 1, "VEC3": 3, "VEC4": 4, "MAT4": 16}
DTYPES = {5126: "<f4", 5123: "<u2", 5121: "u1", 5125: "<u4"}


def accessor(glb: Glb, index: int) -> np.ndarray:
    acc = glb.doc["accessors"][index]
    view = glb.doc["bufferViews"][acc["bufferView"]]
    width = COMPONENTS[acc["type"]]
    start = view.get("byteOffset", 0) + acc.get("byteOffset", 0)
    dtype = np.dtype(DTYPES[acc["componentType"]])
    stride = view.get("byteStride", 0)
    if stride and stride != dtype.itemsize * width:
        rows = [np.frombuffer(glb.bin, dtype, width, start + i * stride) for i in range(acc["count"])]
        return np.array(rows)
    data = np.frombuffer(glb.bin, dtype=dtype, count=acc["count"] * width, offset=start)
    return data.reshape(acc["count"], width)


def _trs_matrix(translation, rotation, scale) -> np.ndarray:
    x, y, z, w = rotation
    r = np.array(
        [
            [1 - 2 * (y * y + z * z), 2 * (x * y - z * w), 2 * (x * z + y * w)],
            [2 * (x * y + z * w), 1 - 2 * (x * x + z * z), 2 * (y * z - x * w)],
            [2 * (x * z - y * w), 2 * (y * z + x * w), 1 - 2 * (x * x + y * y)],
        ]
    )
    m = np.eye(4)
    m[:3, :3] = r * np.asarray(scale)
    m[:3, 3] = translation
    return m


def world_matrices(glb: Glb, frame: int | None = None) -> dict[int, np.ndarray]:
    """World matrix of every node. With frame=None the rest pose is used;
    otherwise the document's first animation is sampled at that keyframe."""
    nodes = glb.doc["nodes"]
    pose = [
        {
            "translation": np.array(n.get("translation", [0, 0, 0]), dtype=float),
            "rotation": np.array(n.get("rotation", [0, 0, 0, 1]), dtype=float),
            "scale": np.array(n.get("scale", [1, 1, 1]), dtype=float),
        }
        for n in nodes
    ]
    if frame is not None:
        animation = glb.doc["animations"][0]
        for channel in animation["channels"]:
            sampler = animation["samplers"][channel["sampler"]]
            values = accessor(glb, sampler["output"])
            pose[channel["target"]["node"]][channel["target"]["path"]] = values[frame].astype(float)

    parent = {child: i for i, n in enumerate(nodes) for child in n.get("children", [])}
    cache: dict[int, np.ndarray] = {}

    def world(i: int) -> np.ndarray:
        if i not in cache:
            local = _trs_matrix(**pose[i])
            cache[i] = world(parent[i]) @ local if i in parent else local
        return cache[i]

    return {i: world(i) for i in range(len(nodes))}


def joint_positions(glb: Glb, names: list[str], frame: int | None = None) -> np.ndarray:
    by_name = {n.get("name"): i for i, n in enumerate(glb.doc["nodes"])}
    matrices = world_matrices(glb, frame)
    return np.array([matrices[by_name[name]][:3, 3] for name in names])
