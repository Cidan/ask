package main

import (
	"encoding/json"
	"reflect"
	"testing"
)

// mustJSONList decodes a JSON array literal so the skills tests can
// feed realistic response shapes without splatting json.Unmarshal
// boilerplate on every case.
func mustJSONList(t *testing.T, raw string) []any {
	t.Helper()
	var out []any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("bad JSON %q: %v", raw, err)
	}
	return out
}

func TestCodexParseSkillsList_FlattensEntries(t *testing.T) {
	// A single cwd with two skills: both should surface with their
	// names and descriptions.
	data := mustJSONList(t, `[
		{"cwd":"/work","errors":[],"skills":[
			{"name":"deploy","description":"deploy to prod","enabled":true,"path":"/s","scope":"repo"},
			{"name":"lint","description":"run lint","enabled":true,"path":"/s","scope":"repo"}
		]}
	]`)
	got := codexParseSkillsList(data)
	want := []providerSlashEntry{
		{Name: "deploy", Description: "deploy to prod"},
		{Name: "lint", Description: "run lint"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v want %+v", got, want)
	}
}

func TestCodexParseSkillsList_SkipsDisabled(t *testing.T) {
	data := mustJSONList(t, `[
		{"cwd":"/work","errors":[],"skills":[
			{"name":"on","description":"on","enabled":true,"path":"/s","scope":"repo"},
			{"name":"off","description":"off","enabled":false,"path":"/s","scope":"repo"}
		]}
	]`)
	got := codexParseSkillsList(data)
	if len(got) != 1 {
		t.Fatalf("want 1 entry (enabled only), got %d: %+v", len(got), got)
	}
	if got[0].Name != "on" {
		t.Errorf("name=%q want 'on'", got[0].Name)
	}
}

func TestCodexParseSkillsList_DedupesAcrossEntries(t *testing.T) {
	// Same skill visible from repo + user scope shouldn't appear
	// twice in the picker.
	data := mustJSONList(t, `[
		{"cwd":"/work","errors":[],"skills":[
			{"name":"deploy","description":"repo deploy","enabled":true,"path":"/r","scope":"repo"}
		]},
		{"cwd":"/home/u","errors":[],"skills":[
			{"name":"deploy","description":"user deploy","enabled":true,"path":"/u","scope":"user"},
			{"name":"clean","description":"","enabled":true,"path":"/u","scope":"user"}
		]}
	]`)
	got := codexParseSkillsList(data)
	if len(got) != 2 {
		t.Fatalf("want 2 deduped entries, got %d: %+v", len(got), got)
	}
	if got[0].Name != "deploy" || got[0].Description != "repo deploy" {
		t.Errorf("first-seen should win (repo deploy), got %+v", got[0])
	}
	if got[1].Name != "clean" {
		t.Errorf("second entry=%+v want name=clean", got[1])
	}
}

func TestCodexParseSkillsList_FallsBackToShortDescription(t *testing.T) {
	// SKILL.md's shortDescription is the legacy field; newer skills
	// carry it in `description`. Either one should populate the
	// modal hint.
	data := mustJSONList(t, `[
		{"cwd":"/w","errors":[],"skills":[
			{"name":"legacy","shortDescription":"via shortDescription","enabled":true,"path":"/s","scope":"repo"}
		]}
	]`)
	got := codexParseSkillsList(data)
	if len(got) != 1 {
		t.Fatalf("want 1 entry, got %d", len(got))
	}
	if got[0].Description != "via shortDescription" {
		t.Errorf("description fallback missing: got %q", got[0].Description)
	}
}

func TestCodexParseSkillsList_SkipsEmptyNames(t *testing.T) {
	data := mustJSONList(t, `[
		{"cwd":"/w","errors":[],"skills":[
			{"name":"","description":"no name","enabled":true,"path":"/s","scope":"repo"},
			{"name":"ok","description":"named","enabled":true,"path":"/s","scope":"repo"}
		]}
	]`)
	got := codexParseSkillsList(data)
	if len(got) != 1 || got[0].Name != "ok" {
		t.Errorf("should keep only named skills, got %+v", got)
	}
}

func TestCodexParseSkillsList_IgnoresMalformedEntries(t *testing.T) {
	data := mustJSONList(t, `[
		"not an object",
		{"cwd":"/w","skills":"not a list"},
		{"cwd":"/w","skills":[
			{"name":"valid","description":"works","enabled":true,"path":"/s","scope":"repo"}
		]}
	]`)
	got := codexParseSkillsList(data)
	if len(got) != 1 || got[0].Name != "valid" {
		t.Errorf("malformed entries should be skipped, got %+v", got)
	}
}

func TestCodexParseSkillsList_EnabledDefaultsToTrueWhenOmitted(t *testing.T) {
	// The SkillMetadata schema requires `enabled`, but older servers
	// could ship records without it. The parser treats missing as
	// "enabled" rather than hiding every skill on a stale server.
	data := mustJSONList(t, `[
		{"cwd":"/w","errors":[],"skills":[
			{"name":"legacy","description":"no enabled field","path":"/s","scope":"repo"}
		]}
	]`)
	got := codexParseSkillsList(data)
	if len(got) != 1 {
		t.Errorf("missing enabled should default to keep, got %+v", got)
	}
}
