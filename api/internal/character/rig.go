// Package character lets users upload their own GLB character, provided it is
// rigged to the Rigforge skeleton. The worker applies motion by joint name, so
// a character with any other skeleton cannot be animated and is rejected here.
package character

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Joint is one joint of the Rigforge skeleton: its name and its parent's name
// ("" for the root).
type Joint struct {
	Name   string
	Parent string
}

// RigJoints is the Rigforge skeleton. The source of truth is
// worker/rig/rigforge_skeleton.json; a test fails if this copy drifts from it.
var RigJoints = []Joint{
	{"hips", ""},
	{"spine", "hips"},
	{"chest", "spine"},
	{"neck", "chest"},
	{"head", "neck"},
	{"left_shoulder", "chest"},
	{"left_elbow", "left_shoulder"},
	{"left_wrist", "left_elbow"},
	{"right_shoulder", "chest"},
	{"right_elbow", "right_shoulder"},
	{"right_wrist", "right_elbow"},
	{"left_hip", "hips"},
	{"left_knee", "left_hip"},
	{"left_ankle", "left_knee"},
	{"right_hip", "hips"},
	{"right_knee", "right_hip"},
	{"right_ankle", "right_knee"},
}

// RigError explains exactly why a GLB is not a Rigforge character. The
// message is shown to the user as-is.
type RigError struct {
	Reason string
}

func (e *RigError) Error() string { return e.Reason }

func rigErrorf(format string, args ...any) error {
	return &RigError{Reason: fmt.Sprintf(format, args...)}
}

// The subset of glTF that validation needs.
type gltfDoc struct {
	Nodes []struct {
		Name     string `json:"name"`
		Children []int  `json:"children"`
		Mesh     *int   `json:"mesh"`
		Skin     *int   `json:"skin"`
	} `json:"nodes"`
	Skins []struct {
		Joints []int `json:"joints"`
	} `json:"skins"`
	Meshes []struct {
		Primitives []struct {
			Attributes map[string]int `json:"attributes"`
		} `json:"primitives"`
	} `json:"meshes"`
}

const (
	glbMagic     = 0x46546C67 // "glTF"
	glbChunkJSON = 0x4E4F534A // "JSON"
)

// parseGLB extracts the JSON chunk of a binary glTF file. Layout: a 12-byte
// header (magic, version, total length) followed by chunks, each a 4-byte
// length, a 4-byte type and the payload; the first chunk must be JSON.
func parseGLB(data []byte) (*gltfDoc, error) {
	if len(data) < 20 || binary.LittleEndian.Uint32(data[0:4]) != glbMagic {
		return nil, rigErrorf("file is not a binary glTF (.glb)")
	}
	if v := binary.LittleEndian.Uint32(data[4:8]); v != 2 {
		return nil, rigErrorf("unsupported glTF version %d (need 2)", v)
	}
	chunkLen := binary.LittleEndian.Uint32(data[12:16])
	if binary.LittleEndian.Uint32(data[16:20]) != glbChunkJSON || uint64(20)+uint64(chunkLen) > uint64(len(data)) {
		return nil, rigErrorf("GLB is malformed: first chunk is not valid JSON")
	}
	var doc gltfDoc
	if err := json.NewDecoder(bytes.NewReader(data[20 : 20+chunkLen])).Decode(&doc); err != nil {
		return nil, rigErrorf("GLB is malformed: %v", err)
	}
	return &doc, nil
}

// ValidateRig checks that a GLB is usable as a Rigforge character:
//  1. it has a skin whose joints are exactly the 17 Rigforge joints by name,
//  2. those joints are parented the same way as the Rigforge skeleton,
//  3. a mesh is skinned to that skin (has JOINTS_0 and WEIGHTS_0).
//
// Bone lengths and proportions are free: retargeting produces rotations, so
// the motion adapts to whatever proportions the character has.
func ValidateRig(data []byte) error {
	doc, err := parseGLB(data)
	if err != nil {
		return err
	}
	if len(doc.Skins) == 0 {
		return rigErrorf("character has no skin; it must be skinned to the Rigforge skeleton")
	}

	// Parent of every node, by index (-1 = scene root).
	parent := make([]int, len(doc.Nodes))
	for i := range parent {
		parent[i] = -1
	}
	for i, n := range doc.Nodes {
		for _, child := range n.Children {
			if child < 0 || child >= len(doc.Nodes) {
				return rigErrorf("GLB is malformed: node %d has an invalid child index", i)
			}
			parent[child] = i
		}
	}

	var firstErr error
	for skinIndex, skin := range doc.Skins {
		err := validateSkin(doc, parent, skinIndex, skin.Joints)
		if err == nil {
			return nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func validateSkin(doc *gltfDoc, parent []int, skinIndex int, joints []int) error {
	// Map joint name -> node index for this skin.
	nodeOf := map[string]int{}
	for _, idx := range joints {
		if idx < 0 || idx >= len(doc.Nodes) {
			return rigErrorf("GLB is malformed: skin references a missing node")
		}
		nodeOf[doc.Nodes[idx].Name] = idx
	}

	var missing, unexpected []string
	want := map[string]bool{}
	for _, j := range RigJoints {
		want[j.Name] = true
		if _, ok := nodeOf[j.Name]; !ok {
			missing = append(missing, j.Name)
		}
	}
	for name := range nodeOf {
		if !want[name] {
			unexpected = append(unexpected, name)
		}
	}
	if len(missing) > 0 || len(unexpected) > 0 || len(joints) != len(RigJoints) {
		slices.Sort(unexpected)
		var parts []string
		if len(missing) > 0 {
			parts = append(parts, "missing joints: "+strings.Join(missing, ", "))
		}
		if len(unexpected) > 0 {
			parts = append(parts, "unexpected joints: "+strings.Join(quoteAll(unexpected), ", "))
		}
		if len(parts) == 0 {
			parts = append(parts, "duplicate joint names")
		}
		return rigErrorf("skeleton does not match the Rigforge skeleton (%d joints expected, %d found); %s",
			len(RigJoints), len(joints), strings.Join(parts, "; "))
	}

	// Hierarchy: walking up from a joint, the first ancestor that is itself a
	// joint must be the Rigforge parent. Non-joint nodes in between (exporters
	// often add an "Armature" node) are fine.
	isJoint := map[int]string{}
	for name, idx := range nodeOf {
		isJoint[idx] = name
	}
	for _, j := range RigJoints {
		got := ""
		for p := parent[nodeOf[j.Name]]; p != -1; p = parent[p] {
			if name, ok := isJoint[p]; ok {
				got = name
				break
			}
		}
		if got != j.Parent {
			return rigErrorf("joint %q must be a child of %q but is a child of %q",
				j.Name, orRoot(j.Parent), orRoot(got))
		}
	}

	// A mesh must actually be bound to this skin with skinning attributes.
	for _, n := range doc.Nodes {
		if n.Skin == nil || *n.Skin != skinIndex || n.Mesh == nil || *n.Mesh < 0 || *n.Mesh >= len(doc.Meshes) {
			continue
		}
		for _, prim := range doc.Meshes[*n.Mesh].Primitives {
			_, hasJoints := prim.Attributes["JOINTS_0"]
			_, hasWeights := prim.Attributes["WEIGHTS_0"]
			if hasJoints && hasWeights {
				return nil
			}
		}
	}
	return rigErrorf("no mesh is skinned to the skeleton (need JOINTS_0 and WEIGHTS_0 vertex attributes)")
}

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = fmt.Sprintf("%q", n)
	}
	return out
}

func orRoot(name string) string {
	if name == "" {
		return "(root)"
	}
	return name
}

// IsRigError reports whether err is a validation failure (the user's file is
// wrong) as opposed to an internal error.
func IsRigError(err error) bool {
	var re *RigError
	return errors.As(err, &re)
}
