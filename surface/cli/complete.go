package cli

import (
	"sort"
	"sync"

	"github.com/rancher/rancher-cli/internal/config"
	"github.com/rancher/rancher-cli/internal/runtime"
)

var (
	storedConfigMu  sync.RWMutex
	storedConfig    *config.Config
	storedConfigDir string
)

// setStoredConfig stores the config loaded by the root Before hook for use
// by completion callbacks (AC-19: one config load per TAB invocation).
func setStoredConfig(cfg *config.Config, dir string) {
	storedConfigMu.Lock()
	defer storedConfigMu.Unlock()
	storedConfig = cfg
	storedConfigDir = dir
}

// storedConfigSnapshot returns the stored config pointer under the read lock,
// or nil if not yet loaded.
func storedConfigSnapshot() *config.Config {
	storedConfigMu.RLock()
	defer storedConfigMu.RUnlock()
	return storedConfig
}

// loadedConfigView returns the stored ConfigView and true when the config has
// been loaded, or a zero view and false when no config is loaded yet.
// Callers that only need a ConfigView regardless of loaded state can ignore the bool.
func loadedConfigView() (runtime.ConfigView, bool) {
	storedConfigMu.RLock()
	defer storedConfigMu.RUnlock()
	if storedConfig == nil {
		return zeroConfigView{}, false
	}
	return newConfigView(storedConfig, storedConfigDir), true
}

// configView wraps a loaded *config.Config as a runtime.ConfigView.
type configView struct {
	ctxNames   []string
	srvNames   []string
	currentCtx string
	currentURL string
	dir        string
}

func newConfigView(cfg *config.Config, dir string) runtime.ConfigView {
	ctxNames := make([]string, 0, len(cfg.Contexts))
	for k := range cfg.Contexts {
		ctxNames = append(ctxNames, k)
	}
	sort.Strings(ctxNames)

	srvNames := make([]string, 0, len(cfg.Servers))
	for k := range cfg.Servers {
		srvNames = append(srvNames, k)
	}
	sort.Strings(srvNames)

	currentURL := ""
	if ctx, ok := cfg.Contexts[cfg.CurrentContext]; ok {
		if srv, ok := cfg.Servers[ctx.Server]; ok {
			currentURL = srv.URL
		}
	}
	return configView{
		ctxNames:   ctxNames,
		srvNames:   srvNames,
		currentCtx: cfg.CurrentContext,
		currentURL: currentURL,
		dir:        dir,
	}
}

func (v configView) ContextNames() []string   { return v.ctxNames }
func (v configView) ServerNames() []string    { return v.srvNames }
func (v configView) CurrentContext() string   { return v.currentCtx }
func (v configView) CurrentServerURL() string { return v.currentURL }
func (v configView) ConfigDir() string        { return v.dir }

// zeroConfigView is returned when no config has been loaded yet.
type zeroConfigView struct{}

func (zeroConfigView) ContextNames() []string   { return nil }
func (zeroConfigView) ServerNames() []string    { return nil }
func (zeroConfigView) CurrentContext() string   { return "" }
func (zeroConfigView) CurrentServerURL() string { return "" }
func (zeroConfigView) ConfigDir() string        { return "" }
