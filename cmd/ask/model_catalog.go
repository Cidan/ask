package main

import (
	"context"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
)

// The model catalog is the process-wide cache behind the model picker: each
// provider's live listing plus models.dev, fetched off the UI goroutine and
// read synchronously by Provider.ModelPicker and ModelMetaFor afterwards.

// modelLister is implemented by providers that can enumerate their models
// over the network; the catalog load skips providers without it.
type modelLister interface {
	ListModels(ctx context.Context) ([]string, error)
}

type modelCatalogLoadedMsg struct {
	options      map[string][]string
	errs         map[string]string
	modelsDevErr error
}

var modelCatalogLoadTimeout = 20 * time.Second

var modelCatalog = struct {
	mu      sync.Mutex
	loading bool
	loaded  bool
	options map[string][]string
	errs    map[string]string
}{options: map[string][]string{}, errs: map[string]string{}}

func cachedModelOptions(providerID string) ([]string, bool) {
	modelCatalog.mu.Lock()
	defer modelCatalog.mu.Unlock()
	opts, ok := modelCatalog.options[providerID]
	return append([]string(nil), opts...), ok
}

func cacheModelOptions(options map[string][]string) {
	modelCatalog.mu.Lock()
	defer modelCatalog.mu.Unlock()
	for id, opts := range options {
		modelCatalog.options[id] = append([]string(nil), opts...)
	}
}

// modelCatalogNotes returns the per-provider listing failures from the last
// load, keyed by provider id, for the picker's section headers.
func modelCatalogNotes() map[string]string {
	modelCatalog.mu.Lock()
	defer modelCatalog.mu.Unlock()
	out := make(map[string]string, len(modelCatalog.errs))
	for id, e := range modelCatalog.errs {
		out[id] = e
	}
	return out
}

func resetModelCatalog() {
	modelCatalog.mu.Lock()
	defer modelCatalog.mu.Unlock()
	modelCatalog.loading = false
	modelCatalog.loaded = false
	modelCatalog.options = map[string][]string{}
	modelCatalog.errs = map[string]string{}
}

// modelCatalogRefreshCmd returns the load command, or nil while a load is
// already running or when one has already succeeded and force is false.
func modelCatalogRefreshCmd(force bool) tea.Cmd {
	modelCatalog.mu.Lock()
	defer modelCatalog.mu.Unlock()
	if modelCatalog.loading || (modelCatalog.loaded && !force) {
		return nil
	}
	modelCatalog.loading = true
	return loadModelCatalogCmd(append([]Provider(nil), providerRegistry...))
}

// loadModelCatalogCmd fetches models.dev and every lister's ids concurrently,
// installs the results in the cache, then reports them. The msg is a
// notification — by the time it lands, ModelPicker already serves the data.
func loadModelCatalogCmd(provs []Provider) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), modelCatalogLoadTimeout)
		defer cancel()

		msg := modelCatalogLoadedMsg{options: map[string][]string{}, errs: map[string]string{}}
		var mu sync.Mutex
		var wg sync.WaitGroup

		wg.Add(1)
		go func() {
			defer wg.Done()
			err := loadModelsDev(ctx)
			mu.Lock()
			msg.modelsDevErr = err
			mu.Unlock()
		}()
		for _, p := range provs {
			lister, ok := p.(modelLister)
			if !ok {
				continue
			}
			wg.Add(1)
			go func(id string, lister modelLister) {
				defer wg.Done()
				ids, err := lister.ListModels(ctx)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					msg.errs[id] = err.Error()
					return
				}
				if len(ids) > 0 {
					msg.options[id] = ids
				}
			}(p.ID(), lister)
		}
		wg.Wait()

		if msg.modelsDevErr != nil {
			debugLog("model catalog: models.dev: %v", msg.modelsDevErr)
		}
		for id, e := range msg.errs {
			debugLog("model catalog: %s listing: %s", id, e)
		}
		finishModelCatalogLoad(msg)
		return msg
	}
}

func finishModelCatalogLoad(msg modelCatalogLoadedMsg) {
	cacheModelOptions(msg.options)
	modelCatalog.mu.Lock()
	defer modelCatalog.mu.Unlock()
	modelCatalog.loading = false
	modelCatalog.loaded = msg.modelsDevErr == nil
	modelCatalog.errs = map[string]string{}
	for id, e := range msg.errs {
		modelCatalog.errs[id] = e
	}
}
