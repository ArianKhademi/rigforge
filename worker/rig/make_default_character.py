"""Generate the default Rigforge character: a mannequin skinned to the
Rigforge skeleton.

The character is built from code, not downloaded, so its origin and licence
are never in question and it can be regenerated at any time:

    blender --background --python worker/rig/make_default_character.py
    # or, with Blender installed as a Python module (pip install bpy):
    python worker/rig/make_default_character.py

It reads rigforge_skeleton.json (next to this file) and writes
default_character.glb (next to this file, or to the path given after "--").

The mesh is a set of simple shapes, one group per bone: capsules for limbs,
ellipsoids for torso, head, hands and feet, and a sphere at every joint.
Each vertex is bound to exactly one bone with weight 1 (rigid skinning). The
joint spheres hide the seams where two rigid parts meet, the way a wooden
artist's mannequin does.
"""

import json
import sys
from pathlib import Path

import bpy  # isort: skip  (must come first: bmesh and mathutils only exist once bpy is loaded)
import bmesh  # isort: skip
from mathutils import Matrix, Vector  # isort: skip

HERE = Path(__file__).resolve().parent
BODY_COLOR = (0.82, 0.80, 0.76, 1.0)
ACCENT_COLOR = (0.93, 0.38, 0.13, 1.0)


def to_blender(p):
    """glTF space (+Y up, +Z forward) -> Blender space (+Z up, -Y forward).
    The glTF exporter applies the inverse on the way out."""
    return Vector((p[0], -p[2], p[1]))


def main():
    args = sys.argv[sys.argv.index("--") + 1 :] if "--" in sys.argv else []
    out_path = Path(args[0]) if args else HERE / "default_character.glb"
    joints = json.loads((HERE / "rigforge_skeleton.json").read_text())["joints"]
    names = [j["name"] for j in joints]
    pos = {j["name"]: to_blender(j["position"]) for j in joints}
    end = {j["name"]: to_blender(j["end"]) for j in joints if "end" in j}

    bpy.ops.wm.read_factory_settings(use_empty=True)

    # ---- Armature: one bone per joint ---------------------------------
    armature_data = bpy.data.armatures.new("Rigforge")
    armature = bpy.data.objects.new("Armature", armature_data)
    bpy.context.scene.collection.objects.link(armature)
    bpy.context.view_layer.objects.active = armature
    bpy.ops.object.mode_set(mode="EDIT")

    # A Blender bone runs from head to tail. The head is the joint; the tail
    # only sets the bone's direction and length. It points at the next joint
    # of the chain (or the bone tip for leaves). The exported glTF node sits
    # at the head, which is all the Rigforge skeleton defines.
    tail_target = {
        "hips": "spine", "spine": "chest", "chest": "neck", "neck": "head",
        "left_shoulder": "left_elbow", "left_elbow": "left_wrist",
        "right_shoulder": "right_elbow", "right_elbow": "right_wrist",
        "left_hip": "left_knee", "left_knee": "left_ankle",
        "right_hip": "right_knee", "right_knee": "right_ankle",
    }  # fmt: skip
    bones = {}
    for joint in joints:
        name = joint["name"]
        bone = armature_data.edit_bones.new(name)
        bone.head = pos[name]
        bone.tail = pos[tail_target[name]] if name in tail_target else end[name]
        bone.roll = 0.0
        if joint["parent"] >= 0:
            bone.parent = bones[names[joint["parent"]]]
            bone.use_connect = False  # keep the head exactly at the joint position
        bones[name] = bone
    bpy.ops.object.mode_set(mode="OBJECT")

    # ---- Mesh ----------------------------------------------------------
    mesh = bpy.data.meshes.new("Mannequin")
    body = bpy.data.objects.new("Mannequin", mesh)
    bpy.context.scene.collection.objects.link(body)
    for label, color in (("Body", BODY_COLOR), ("Accent", ACCENT_COLOR)):
        material = bpy.data.materials.new(label)
        material.use_nodes = True
        shader = material.node_tree.nodes.get("Principled BSDF")
        shader.inputs["Base Color"].default_value = color
        shader.inputs["Roughness"].default_value = 0.6
        shader.inputs["Metallic"].default_value = 0.0
        mesh.materials.append(material)
    BODY, ACCENT = 0, 1
    group_index = {name: body.vertex_groups.new(name=name).index for name in names}

    bm = bmesh.new()
    weights = bm.verts.layers.deform.verify()  # per-vertex {vertex group: weight}

    def bind(verts, bone, material):
        """Bind new vertices rigidly to one bone and style their faces."""
        for v in verts:
            v[weights][group_index[bone]] = 1.0
        for face in {f for v in verts for f in v.link_faces}:
            face.material_index = material
            face.smooth = True

    def ellipsoid(center, radii, bone, material=BODY):
        verts = bmesh.ops.create_uvsphere(bm, u_segments=24, v_segments=14, radius=1.0)["verts"]
        for v in verts:
            v.co = Vector(center) + Vector((v.co.x * radii[0], v.co.y * radii[1], v.co.z * radii[2]))
        bind(verts, bone, material)

    def sphere(center, radius, bone, material=ACCENT):
        ellipsoid(center, (radius, radius, radius), bone, material)

    def capsule(a, b, radius, bone, material=BODY):
        """A cylinder from a to b with a rounded cap at b (the joint sphere of
        this bone covers the end at a)."""
        a, b = Vector(a), Vector(b)
        # create_cone builds along +Z around the origin; rotate +Z onto the
        # bone direction and move it to the midpoint.
        placement = Matrix.Translation((a + b) / 2) @ Vector((0, 0, 1)).rotation_difference(b - a).to_matrix().to_4x4()
        verts = bmesh.ops.create_cone(
            bm, cap_ends=True, segments=20, radius1=radius, radius2=radius, depth=(b - a).length, matrix=placement
        )["verts"]
        bind(verts, bone, material)
        sphere(b, radius, bone, material)

    def at(x, y, z):
        """A point given in glTF coordinates (metres, y up, z forward)."""
        return to_blender((x, y, z))

    # Radii below are (left-right, front-back, up-down) in metres.
    # Torso and head, bottom to top.
    ellipsoid(at(0, 0.955, 0), (0.150, 0.100, 0.105), "hips")
    ellipsoid(at(0, 1.180, 0), (0.135, 0.090, 0.130), "spine")
    ellipsoid(at(0, 1.360, 0), (0.175, 0.105, 0.130), "chest")
    capsule(pos["neck"] - Vector((0, 0, 0.03)), pos["head"], 0.045, "neck")
    ellipsoid(at(0, 1.665, 0.005), (0.085, 0.100, 0.110), "head")
    # A visor on the front of the head, so it is obvious which way it faces.
    ellipsoid(at(0, 1.680, 0.060), (0.070, 0.050, 0.032), "head", ACCENT)

    for side, sign in (("left", 1.0), ("right", -1.0)):
        shoulder, elbow, wrist = (pos[f"{side}_{part}"] for part in ("shoulder", "elbow", "wrist"))
        hip, knee, ankle = (pos[f"{side}_{part}"] for part in ("hip", "knee", "ankle"))

        # Arm: ball joint, then the segment it drives.
        sphere(shoulder, 0.058, f"{side}_shoulder")
        capsule(shoulder, elbow, 0.044, f"{side}_shoulder")
        sphere(elbow, 0.047, f"{side}_elbow")
        capsule(elbow, wrist, 0.037, f"{side}_elbow")
        sphere(wrist, 0.038, f"{side}_wrist")
        # Hand: a flat ellipsoid, palm down in the T-pose.
        ellipsoid(wrist + Vector((sign * 0.075, 0, 0)), (0.070, 0.042, 0.020), f"{side}_wrist")

        # Leg.
        sphere(hip, 0.078, f"{side}_hip")
        capsule(hip, knee, 0.066, f"{side}_hip")
        sphere(knee, 0.060, f"{side}_knee")
        capsule(knee, ankle, 0.048, f"{side}_knee")
        sphere(ankle, 0.050, f"{side}_ankle")
        # Foot: from just behind the ankle to the toe tip, resting on y = 0.
        ellipsoid(at(sign * 0.10, 0.042, 0.065), (0.052, 0.125, 0.042), f"{side}_ankle")

    bm.to_mesh(mesh)
    bm.free()

    # ---- Skin the mesh to the armature ---------------------------------
    body.parent = armature
    modifier = body.modifiers.new("Armature", "ARMATURE")
    modifier.object = armature

    # ---- Export --------------------------------------------------------
    bpy.ops.export_scene.gltf(
        filepath=str(out_path),
        export_format="GLB",
        export_yup=True,  # convert to glTF's +Y up
        export_skins=True,
        export_animations=False,
        export_apply=False,  # keep the armature modifier as a skin, do not bake it
        export_texcoords=False,
        export_normals=True,
    )
    print(f"wrote {out_path} ({out_path.stat().st_size} bytes)")


if __name__ == "__main__":
    main()
