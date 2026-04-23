package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestCodexParseModelList_PromotesDefaultThenRest(t *testing.T) {
	raw := []byte(`[
		{"id":"gpt-5-fast","model":"gpt-5","displayName":"GPT-5 Fast","hidden":false,"isDefault":false},
		{"id":"gpt-5","model":"gpt-5","displayName":"GPT-5","hidden":false,"isDefault":true},
		{"id":"hidden-model","model":"hm","displayName":"hidden","hidden":true,"isDefault":false},
		{"id":"o3","model":"o3","displayName":"O3","hidden":false,"isDefault":false}
	]`)
	var data []any
	_ = json.Unmarshal(raw, &data)
	got := codexParseModelList(data)
	want := []string{"gpt-5", "gpt-5-fast", "o3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("model ids=%v want %v", got, want)
	}
}

func TestCodexParseModelList_SkipsBadEntries(t *testing.T) {
	// Entries missing id, wrong shape, or hidden=true all drop out.
	raw := []byte(`[
		{"id":"","model":"","displayName":"empty"},
		"string-not-object",
		{"id":"good","model":"g","displayName":"good","hidden":false,"isDefault":false}
	]`)
	var data []any
	_ = json.Unmarshal(raw, &data)
	got := codexParseModelList(data)
	if len(got) != 1 || got[0] != "good" {
		t.Errorf("got %v want [good]", got)
	}
}

func TestCodex_ModelPicker_AlwaysSeedsDefaultAndAllowCustom(t *testing.T) {
	// With no codex binary on PATH (test env) the live fetch fails,
	// and the picker must still return a usable shell: a single
	// "default" row plus AllowCustom so /model can still prompt for
	// a typed model id.
	isolateHome(t)
	t.Setenv("PATH", "/nonexistent")
	var cp codexProvider
	picker := cp.ModelPicker()
	if len(picker.Options) == 0 {
		t.Fatal("ModelPicker must never return an empty option list")
	}
	if picker.Options[0] != "default" {
		t.Errorf("first option=%q want 'default'", picker.Options[0])
	}
	if !picker.AllowCustom {
		t.Error("AllowCustom must stay true so typed ids still work")
	}
}
