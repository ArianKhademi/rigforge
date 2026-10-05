package character_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ArianKhademi/rigforge/api/internal/apitest"
	"github.com/ArianKhademi/rigforge/api/internal/character"
)

// glb builds a minimal binary glTF around a JSON document (no BIN chunk; rig
// validation only reads the JSON).
func glb(doc map[string]any) []byte {
	jsonChunk, _ := json.Marshal(doc)
	for len(jsonChunk)%4 != 0 {
		jsonChunk = append(jsonChunk, ' ')
	}
	var buf bytes.Buffer
	write := func(v uint32) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	write(0x46546C67) // "glTF"
	write(2)
	write(uint32(12 + 8 + len(jsonChunk)))
	write(uint32(len(jsonChunk)))
	write(0x4E4F534A) // "JSON"
	buf.Write(jsonChunk)
	return buf.Bytes()
}

// rigged returns a glTF document with a skinned mesh whose skeleton is the
// given joints. Node 0 is the mesh, node 1 an "Armature" wrapper, and joints
// start at node 2, which mimics what Blender exports.
func rigged(joints []character.Joint) map[string]any {
	const first = 2
	index := map[string]int{}
	for i, j := range joints {
		index[j.Name] = first + i
	}
	nodes := []map[string]any{
		{"name": "Body", "mesh": 0, "skin": 0},
		{"name": "Armature", "children": []int{}},
	}
	skinJoints := []int{}
	for i, j := range joints {
		nodes = append(nodes, map[string]any{"name": j.Name})
		skinJoints = append(skinJoints, first+i)
	}
	addChild := func(parent, child int) {
		kids, _ := nodes[parent]["children"].([]int)
		nodes[parent]["children"] = append(kids, child)
	}
	for i, j := range joints {
		if parent, ok := index[j.Parent]; ok {
			addChild(parent, first+i)
		} else {
			addChild(1, first+i) // root joints hang off the Armature node
		}
	}
	return map[string]any{
		"asset": map[string]any{"version": "2.0"},
		"nodes": nodes,
		"skins": []map[string]any{{"joints": skinJoints}},
		"meshes": []map[string]any{{"primitives": []map[string]any{
			{"attributes": map[string]int{"POSITION": 0, "JOINTS_0": 1, "WEIGHTS_0": 2}},
		}}},
	}
}

func rig() []character.Joint { return append([]character.Joint(nil), character.RigJoints...) }

func TestValidRigIsAccepted(t *testing.T) {
	if err := character.ValidateRig(glb(rigged(rig()))); err != nil {
		t.Fatalf("a correctly rigged character was rejected: %v", err)
	}
}

func TestInvalidRigsAreRejectedWithAClearReason(t *testing.T) {
	missingJoint := rig()[:16] // drops right_ankle

	extraJoint := append(rig(), character.Joint{Name: "tail", Parent: "hips"})

	renamed := rig()
	renamed[4].Name = "Head" // joint names are case-sensitive

	wrongParent := rig()
	wrongParent[6].Parent = "chest" // left_elbow hangs off the chest instead of the shoulder

	noMeshAttrs := rigged(rig())
	noMeshAttrs["meshes"] = []map[string]any{{"primitives": []map[string]any{
		{"attributes": map[string]int{"POSITION": 0}},
	}}}

	noSkin := rigged(rig())
	delete(noSkin, "skins")

	cases := []struct {
		name string
		data []byte
		want string // substring of the message shown to the user
	}{
		{"not a glb", []byte("this is definitely not a glb file"), "not a binary glTF"},
		{"empty file", nil, "not a binary glTF"},
		{"no skin", glb(noSkin), "no skin"},
		{"missing joint", glb(rigged(missingJoint)), "missing joints: right_ankle"},
		{"extra joint", glb(rigged(extraJoint)), `unexpected joints: "tail"`},
		{"renamed joint", glb(rigged(renamed)), "missing joints: head"},
		{"wrong parent", glb(rigged(wrongParent)), `"left_elbow" must be a child of "left_shoulder"`},
		{"mesh not skinned", glb(noMeshAttrs), "JOINTS_0 and WEIGHTS_0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := character.ValidateRig(tc.data)
			if err == nil {
				t.Fatal("accepted an invalid character")
			}
			if !character.IsRigError(err) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestRigJointsMatchWorkerSkeleton guards the one piece of duplicated data in
// the repo: the Go copy of the skeleton must equal the worker's JSON.
func TestRigJointsMatchWorkerSkeleton(t *testing.T) {
	raw, err := os.ReadFile("../../../worker/rig/rigforge_skeleton.json")
	if err != nil {
		t.Fatal(err)
	}
	var skeleton struct {
		Joints []struct {
			Name   string `json:"name"`
			Parent int    `json:"parent"`
		} `json:"joints"`
	}
	if err := json.Unmarshal(raw, &skeleton); err != nil {
		t.Fatal(err)
	}
	if len(skeleton.Joints) != 17 || len(character.RigJoints) != 17 {
		t.Fatalf("skeleton has %d joints in JSON and %d in Go, want 17", len(skeleton.Joints), len(character.RigJoints))
	}
	for i, j := range skeleton.Joints {
		parent := ""
		if j.Parent >= 0 {
			parent = skeleton.Joints[j.Parent].Name
		}
		if got := character.RigJoints[i]; got.Name != j.Name || got.Parent != parent {
			t.Errorf("joint %d: Go has %+v, JSON has {%s %s}", i, got, j.Name, parent)
		}
	}
}

// upload posts a character file as multipart/form-data.
func upload(e *apitest.Env, user, filename string, data []byte) *httptest.ResponseRecorder {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, _ := form.CreateFormFile("file", filename)
	_, _ = part.Write(data)
	_ = form.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/characters", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+e.Token(user))
	rec := httptest.NewRecorder()
	e.Server.Router.ServeHTTP(rec, req)
	return rec
}

type listBody struct {
	Characters []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Builtin bool   `json:"builtin"`
	} `json:"characters"`
}

func TestUploadListAndDelete(t *testing.T) {
	e := apitest.New(t)

	// Before any upload the two built-ins are available.
	list := apitest.Decode[listBody](t, e.Do("alice", http.MethodGet, "/api/characters", nil)).Characters
	if len(list) != 2 || list[0].ID != "default" || list[1].ID != "none" || !list[0].Builtin {
		t.Fatalf("built-ins = %+v", list)
	}

	rec := upload(e, "alice", "robot.glb", glb(rigged(rig())))
	apitest.WantStatus(t, rec, http.StatusCreated)
	created := apitest.Decode[struct{ ID, Name string }](t, rec)
	if created.Name != "robot" {
		t.Fatalf("name = %q, want the file name without extension", created.Name)
	}
	if _, stored := e.Objects.Objects["characters/"+created.ID+".glb"]; !stored {
		t.Fatal("character file was not stored")
	}

	// Listed for its owner, invisible to anyone else.
	if list := apitest.Decode[listBody](t, e.Do("alice", http.MethodGet, "/api/characters", nil)).Characters; len(list) != 3 {
		t.Fatalf("alice sees %d characters, want 3", len(list))
	}
	if list := apitest.Decode[listBody](t, e.Do("bob", http.MethodGet, "/api/characters", nil)).Characters; len(list) != 2 {
		t.Fatalf("bob sees %d characters, want only the 2 built-ins", len(list))
	}

	apitest.WantStatus(t, e.Do("bob", http.MethodDelete, "/api/characters/"+created.ID, nil), http.StatusNotFound)
	apitest.WantStatus(t, e.Do("alice", http.MethodDelete, "/api/characters/"+created.ID, nil), http.StatusNoContent)
	if len(e.Objects.Objects) != 0 {
		t.Fatal("deleting a character must delete its file")
	}
}

func TestUploadRejectsWrongSkeleton(t *testing.T) {
	e := apitest.New(t)
	rec := upload(e, "alice", "mixamo.glb", glb(rigged([]character.Joint{
		{Name: "mixamorig:Hips"}, {Name: "mixamorig:Spine", Parent: "mixamorig:Hips"},
	})))
	apitest.WantStatus(t, rec, http.StatusUnprocessableEntity)
	body := apitest.Decode[struct {
		Error struct{ Code, Message string }
	}](t, rec)
	if body.Error.Code != "invalid_rig" || !strings.Contains(body.Error.Message, "missing joints: hips") {
		t.Fatalf("error = %+v", body.Error)
	}
	if len(e.Objects.Objects) != 0 {
		t.Fatal("a rejected character must not be stored")
	}
}
