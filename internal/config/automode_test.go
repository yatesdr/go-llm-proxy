package config

import (
	"os"
	"strings"
	"testing"
)

const autoModeFixture = `listen: ":8080"
models:
  - name: glm
    model: GLM-5.3-Flash
    backend: http://localhost:8000/v1
  - name: claude
    type: anthropic
    backend: https://api.anthropic.com
aliases:
  fast: glm
auto_mode:
  model: glm
  block_probability: 0.4
`

func TestAutoModeLoad(t *testing.T) {
	cs := loadStore(t, writeFixture(t, autoModeFixture))
	cfg := cs.Get()
	if cfg.AutoMode == nil || cfg.AutoMode.Model != "glm" || cfg.AutoMode.Threshold() != 0.4 {
		t.Fatalf("auto_mode = %+v", cfg.AutoMode)
	}
	if m := cfg.AutoModeModel(); m == nil || m.Model != "GLM-5.3-Flash" {
		t.Fatalf("AutoModeModel = %+v", m)
	}
}

func TestAutoModeAbsentMeansOff(t *testing.T) {
	cfg := loadStore(t, writeFixture(t, baseFixture)).Get()
	if cfg.AutoMode != nil || cfg.AutoModeModel() != nil {
		t.Fatal("no auto_mode block must mean no interception")
	}
	var a *AutoModeConfig
	if a.Threshold() != 0.5 {
		t.Fatalf("default threshold = %v", a.Threshold())
	}
}

func TestAutoModeAcceptsAlias(t *testing.T) {
	fixture := strings.Replace(autoModeFixture, "  model: glm\n", "  model: fast\n", 1)
	cfg := loadStore(t, writeFixture(t, fixture)).Get()
	if m := cfg.AutoModeModel(); m == nil || m.Name != "glm" {
		t.Fatalf("alias not resolved: %+v", m)
	}
}

func TestAutoModeValidation(t *testing.T) {
	cases := map[string]string{
		"unknown model":     strings.Replace(autoModeFixture, "  model: glm\n", "  model: nope\n", 1),
		"anthropic backend": strings.Replace(autoModeFixture, "  model: glm\n", "  model: claude\n", 1),
		"probability high":  strings.Replace(autoModeFixture, "0.4", "1.5", 1),
		"probability low":   strings.Replace(autoModeFixture, "0.4", "-0.1", 1),
		"empty model":       strings.Replace(autoModeFixture, "  model: glm\n", "  model: \"\"\n", 1),
	}
	for name, fixture := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewConfigStore(writeFixture(t, fixture)); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestUpdateAutoModeRoundTrip(t *testing.T) {
	path := writeFixture(t, baseFixture)
	cs := loadStore(t, path)
	if err := cs.UpdateAutoMode("model-b", 0.3); err != nil {
		t.Fatal(err)
	}
	cfg := cs.Get()
	if cfg.AutoMode == nil || cfg.AutoMode.Model != "model-b" || cfg.AutoMode.Threshold() != 0.3 {
		t.Fatalf("after set: %+v", cfg.AutoMode)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "auto_mode:") || !strings.Contains(string(data), "# test config") {
		t.Fatalf("saved file lost content:\n%s", data)
	}
	if err := cs.UpdateAutoMode("nope", 0); err == nil {
		t.Fatal("unknown model must be refused")
	}
	if err := cs.UpdateAutoMode("", 0); err != nil {
		t.Fatal(err)
	}
	if cs.Get().AutoMode != nil {
		t.Fatal("empty model must remove the block")
	}
}

func TestAutoModeFollowsModelRename(t *testing.T) {
	cs := loadStore(t, writeFixture(t, autoModeFixture))
	m := *FindModel(cs.Get(), "glm")
	m.Name = "glm-flash"
	if err := cs.UpdateModel("glm", m); err != nil {
		t.Fatal(err)
	}
	if got := cs.Get().AutoMode; got == nil || got.Model != "glm-flash" {
		t.Fatalf("auto_mode after rename: %+v", got)
	}
}

func TestAutoModeBlocksModelDeleteUnlessForced(t *testing.T) {
	path := writeFixture(t, strings.Replace(autoModeFixture, "aliases:\n  fast: glm\n", "", 1))
	cs := loadStore(t, path)
	if err := cs.DeleteModel("glm", false); err == nil || !strings.Contains(err.Error(), "auto_mode.model") {
		t.Fatalf("delete should name the referrer, got %v", err)
	}
	if err := cs.DeleteModel("glm", true); err != nil {
		t.Fatal(err)
	}
	if cs.Get().AutoMode != nil {
		t.Fatal("force delete must drop the auto_mode block")
	}
}
