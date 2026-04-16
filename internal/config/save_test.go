package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const baseFixture = `# test config
listen: ":8080"

# ── Models ────────────────────────────────────────────
models:
  - name: model-a
    backend: http://localhost:8000/v1
  - name: model-b
    backend: http://localhost:8001/v1

# ── API Keys ──────────────────────────────────────────
keys:
  - key: sk-existing-one
    name: first
    models: [model-a]
  - key: sk-existing-two
    name: second
`

func writeFixture(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}

func loadStore(t *testing.T, path string) *ConfigStore {
	t.Helper()
	cs, err := NewConfigStore(path)
	if err != nil {
		t.Fatalf("NewConfigStore: %v", err)
	}
	return cs
}

func TestKeyHashStableAcrossCalls(t *testing.T) {
	h1 := KeyHash("sk-hello")
	h2 := KeyHash("sk-hello")
	if h1 != h2 {
		t.Fatalf("KeyHash not stable: %q vs %q", h1, h2)
	}
	if KeyHash("sk-hello") == KeyHash("sk-world") {
		t.Fatal("KeyHash collision on distinct inputs")
	}
	if len(h1) != 16 {
		t.Fatalf("KeyHash should be 16 hex chars, got %d", len(h1))
	}
}

func TestMaskKey(t *testing.T) {
	cases := map[string]string{
		"dy-3855abcdef0123456789":  "dy-3855...89",
		"sk-abcdef":                "***", // too short
		"":                         "***",
		"dy-abcdefghijklmnopqrstu": "dy-abcd...tu",
	}
	for in, want := range cases {
		got := MaskKey(in)
		if got != want {
			t.Errorf("MaskKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGenerateKeyFormat(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if !strings.HasPrefix(k, "dy-") {
		t.Errorf("expected dy- prefix, got %q", k)
	}
	if len(k) != 3+48 { // dy- + 24 bytes × 2 hex chars
		t.Errorf("unexpected key length %d", len(k))
	}
}

func TestAddKeyAppendsAndReturnsFullKey(t *testing.T) {
	path := writeFixture(t, baseFixture)
	cs := loadStore(t, path)

	before := len(cs.Get().Keys)
	key, err := cs.AddKey("derek-laptop")
	if err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	if !strings.HasPrefix(key, "dy-") {
		t.Errorf("generated key should start with dy-, got %q", key)
	}
	after := cs.Get().Keys
	if len(after) != before+1 {
		t.Fatalf("expected %d keys after add, got %d", before+1, len(after))
	}
	last := after[len(after)-1]
	if last.Name != "derek-laptop" || last.Key != key {
		t.Errorf("appended key mismatch: got %+v", last)
	}
}

func TestAddKeyRejectsEmptyName(t *testing.T) {
	path := writeFixture(t, baseFixture)
	cs := loadStore(t, path)
	if _, err := cs.AddKey(""); err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestAddKeyPreservesComments(t *testing.T) {
	path := writeFixture(t, baseFixture)
	cs := loadStore(t, path)
	if _, err := cs.AddKey("newbie"); err != nil {
		t.Fatalf("AddKey: %v", err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	text := string(out)
	for _, marker := range []string{"# test config", "# ── Models ", "# ── API Keys "} {
		if !strings.Contains(text, marker) {
			t.Errorf("comment %q missing after save\n--- file ---\n%s", marker, text)
		}
	}
}

func TestUpdateKeyModelsReplacesList(t *testing.T) {
	path := writeFixture(t, baseFixture)
	cs := loadStore(t, path)

	target := cs.Get().Keys[0]
	hash := KeyHash(target.Key)
	if err := cs.UpdateKeyModels(hash, []string{"model-b"}); err != nil {
		t.Fatalf("UpdateKeyModels: %v", err)
	}
	got := cs.Get().Keys[0]
	if len(got.Models) != 1 || got.Models[0] != "model-b" {
		t.Errorf("expected [model-b], got %v", got.Models)
	}
}

func TestUpdateKeyModelsEmptyClearsRestriction(t *testing.T) {
	path := writeFixture(t, baseFixture)
	cs := loadStore(t, path)

	target := cs.Get().Keys[0]
	hash := KeyHash(target.Key)
	if err := cs.UpdateKeyModels(hash, nil); err != nil {
		t.Fatalf("UpdateKeyModels: %v", err)
	}
	got := cs.Get().Keys[0]
	if len(got.Models) != 0 {
		t.Errorf("expected empty models after clear, got %v", got.Models)
	}
}

func TestUpdateKeyModelsRejectsUnknownModel(t *testing.T) {
	path := writeFixture(t, baseFixture)
	cs := loadStore(t, path)
	hash := KeyHash(cs.Get().Keys[0].Key)
	if err := cs.UpdateKeyModels(hash, []string{"nonexistent"}); err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestRenameKey(t *testing.T) {
	path := writeFixture(t, baseFixture)
	cs := loadStore(t, path)
	hash := KeyHash(cs.Get().Keys[0].Key)
	if err := cs.RenameKey(hash, "first-renamed"); err != nil {
		t.Fatalf("RenameKey: %v", err)
	}
	if cs.Get().Keys[0].Name != "first-renamed" {
		t.Errorf("rename did not take: got %q", cs.Get().Keys[0].Name)
	}
}

func TestDeleteKeyRemovesEntry(t *testing.T) {
	path := writeFixture(t, baseFixture)
	cs := loadStore(t, path)
	hash := KeyHash(cs.Get().Keys[1].Key) // delete the "second" key
	if err := cs.DeleteKey(hash); err != nil {
		t.Fatalf("DeleteKey: %v", err)
	}
	keys := cs.Get().Keys
	if len(keys) != 1 || keys[0].Name != "first" {
		t.Errorf("unexpected keys after delete: %+v", keys)
	}
}

func TestDeleteKeyRefusesLastRemaining(t *testing.T) {
	fixture := `listen: ":8080"
models:
  - name: model-a
    backend: http://localhost:8000/v1
keys:
  - key: sk-only
    name: only
`
	path := writeFixture(t, fixture)
	cs := loadStore(t, path)
	hash := KeyHash("sk-only")
	if err := cs.DeleteKey(hash); err == nil {
		t.Fatal("expected error when deleting last key")
	}
}

func TestAddModelRoundTrip(t *testing.T) {
	path := writeFixture(t, baseFixture)
	cs := loadStore(t, path)
	newModel := ModelConfig{
		Name:           "model-c",
		Backend:        "http://localhost:8002/v1",
		Type:           BackendOpenAI,
		ContextWindow:  32000,
		SupportsVision: true,
	}
	if err := cs.AddModel(newModel); err != nil {
		t.Fatalf("AddModel: %v", err)
	}
	got := FindModel(cs.Get(), "model-c")
	if got == nil {
		t.Fatal("model-c not found after add")
	}
	if got.Backend != newModel.Backend || got.ContextWindow != 32000 || !got.SupportsVision {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestAddModelDuplicateRejected(t *testing.T) {
	path := writeFixture(t, baseFixture)
	cs := loadStore(t, path)
	err := cs.AddModel(ModelConfig{Name: "model-a", Backend: "http://localhost:1/v1"})
	if err == nil {
		t.Fatal("expected duplicate name error")
	}
}

func TestUpdateModelSupportsRename(t *testing.T) {
	path := writeFixture(t, baseFixture)
	cs := loadStore(t, path)
	err := cs.UpdateModel("model-a", ModelConfig{
		Name:    "model-a-renamed",
		Backend: "http://localhost:8000/v1",
	})
	if err == nil {
		// rename breaks "model-a" reference in keys[0].models, should fail validation.
		t.Log("expected validation error since model-a is referenced by 'first' key")
	}
	// Repeat without the dangling reference.
	hash := KeyHash(cs.Get().Keys[0].Key)
	if err := cs.UpdateKeyModels(hash, nil); err != nil {
		t.Fatalf("clearing models: %v", err)
	}
	if err := cs.UpdateModel("model-a", ModelConfig{
		Name:    "model-a-renamed",
		Backend: "http://localhost:8000/v1",
	}); err != nil {
		t.Fatalf("UpdateModel rename: %v", err)
	}
	if FindModel(cs.Get(), "model-a-renamed") == nil {
		t.Error("renamed model not found")
	}
	if FindModel(cs.Get(), "model-a") != nil {
		t.Error("original name should be gone")
	}
}

func TestDeleteModelRefusesWhenReferenced(t *testing.T) {
	path := writeFixture(t, baseFixture)
	cs := loadStore(t, path)
	// model-a is in keys[0].models, so non-force delete should fail.
	err := cs.DeleteModel("model-a", false)
	if err == nil {
		t.Fatal("expected error for referenced model")
	}
	if !strings.Contains(err.Error(), "referenced") {
		t.Errorf("expected 'referenced' in error, got %v", err)
	}
}

func TestDeleteModelForceStripsReferences(t *testing.T) {
	path := writeFixture(t, baseFixture)
	cs := loadStore(t, path)
	if err := cs.DeleteModel("model-a", true); err != nil {
		t.Fatalf("DeleteModel force: %v", err)
	}
	// model-a gone, and keys[0].Models should no longer contain it.
	if FindModel(cs.Get(), "model-a") != nil {
		t.Error("model-a still present after force delete")
	}
	for _, k := range cs.Get().Keys {
		for _, m := range k.Models {
			if m == "model-a" {
				t.Errorf("key %q still references deleted model-a", k.Name)
			}
		}
	}
}

func TestDeleteModelWhenUnreferenced(t *testing.T) {
	path := writeFixture(t, baseFixture)
	cs := loadStore(t, path)
	if err := cs.DeleteModel("model-b", false); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	if FindModel(cs.Get(), "model-b") != nil {
		t.Error("model-b should be gone")
	}
}

func TestLookupKeyByHashUnknown(t *testing.T) {
	path := writeFixture(t, baseFixture)
	cs := loadStore(t, path)
	if err := cs.UpdateKeyModels("deadbeef", []string{"model-a"}); err == nil {
		t.Fatal("expected error for unknown hash")
	}
}
