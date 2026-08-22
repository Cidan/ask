package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// listingProvider is a fake provider that can enumerate models, so the
// catalog load has something to collect.
type listingProvider struct {
	*fakeProvider
	ids []string
	err error
}

func (p listingProvider) ListModels(context.Context) ([]string, error) { return p.ids, p.err }

func stubModelsDev(t *testing.T, err error) {
	t.Helper()
	prev := loadModelsDev
	loadModelsDev = func(context.Context) error { return err }
	t.Cleanup(func() { loadModelsDev = prev })
}

func TestLoadModelCatalogCmd_CollectsListingsAndErrors(t *testing.T) {
	resetModelCatalog()
	t.Cleanup(resetModelCatalog)
	stubModelsDev(t, nil)

	good := newFakeProvider()
	good.id = "good"
	bad := newFakeProvider()
	bad.id = "bad"
	plain := newFakeProvider()
	plain.id = "plain"
	provs := []Provider{
		listingProvider{fakeProvider: good, ids: []string{"b-2", "a-1"}},
		listingProvider{fakeProvider: bad, err: errors.New("boom")},
		plain,
	}

	msg, ok := loadModelCatalogCmd(provs)().(modelCatalogLoadedMsg)
	if !ok {
		t.Fatal("load cmd must yield modelCatalogLoadedMsg")
	}
	if msg.modelsDevErr != nil {
		t.Errorf("models.dev stub succeeded, got %v", msg.modelsDevErr)
	}
	if got := strings.Join(msg.options["good"], ","); got != "b-2,a-1" {
		t.Errorf("listing must be reported verbatim, got %q", got)
	}
	if _, ok := msg.options["bad"]; ok {
		t.Error("a failed listing must not produce options")
	}
	if !strings.Contains(msg.errs["bad"], "boom") {
		t.Errorf("listing failure must be reported, got %v", msg.errs)
	}
	if _, ok := msg.options["plain"]; ok {
		t.Error("providers without ListModels are skipped")
	}

	if opts, ok := cachedModelOptions("good"); !ok || len(opts) != 2 {
		t.Errorf("successful listings must land in the cache before the msg, got ok=%v %v", ok, opts)
	}
	if notes := modelCatalogNotes(); !strings.Contains(notes["bad"], "boom") {
		t.Errorf("listing failures must surface as section notes, got %v", notes)
	}
	if cmd := modelCatalogRefreshCmd(false); cmd != nil {
		t.Error("a completed load (models.dev ok) must not be repeated without force")
	}
	if cmd := modelCatalogRefreshCmd(true); cmd == nil {
		t.Error("force must always return a load cmd")
	}
}

func TestModelCatalogRefreshCmd_RetriesWhileModelsDevFails(t *testing.T) {
	resetModelCatalog()
	t.Cleanup(resetModelCatalog)
	stubModelsDev(t, errors.New("offline"))

	cmd := modelCatalogRefreshCmd(false)
	if cmd == nil {
		t.Fatal("fresh catalog must load")
	}
	if again := modelCatalogRefreshCmd(false); again != nil {
		t.Error("a load in flight must not be dispatched twice")
	}
	msg := cmd().(modelCatalogLoadedMsg)
	if msg.modelsDevErr == nil {
		t.Fatal("stub error must be reported")
	}
	if cmd := modelCatalogRefreshCmd(false); cmd == nil {
		t.Error("a failed models.dev load must be retried on the next open")
	}
}

func TestAgentProvider_ModelPickerServesCachedListing(t *testing.T) {
	resetModelCatalog()
	t.Cleanup(resetModelCatalog)
	p := vertexAgentProvider()

	static := p.ModelPicker().Options
	if len(static) == 0 || static[0] != vertexDefaultModel {
		t.Fatalf("before any load the picker serves the static catalog, got %v", static)
	}
	cacheModelOptions(map[string][]string{vertexProviderID: {"gemini-live-1", "gemini-live-2"}})
	if live := p.ModelPicker().Options; strings.Join(live, ",") != "gemini-live-1,gemini-live-2" {
		t.Errorf("cached listing must win, got %v", live)
	}
	cacheModelOptions(map[string][]string{vertexProviderID: {}})
	if back := p.ModelPicker().Options; len(back) == 0 || back[0] != vertexDefaultModel {
		t.Errorf("an empty cached listing falls back to the static catalog, got %v", back)
	}
}
