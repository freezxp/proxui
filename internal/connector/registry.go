package connector

import (
	"fmt"
	"sort"
	"sync"
)

// Factory builds a connector instance for one configured platform.
type Factory func(cfg Config, creds Credentials, opts Options) (Connector, error)

type registration struct {
	factory Factory
	info    Info
}

var (
	registryMu sync.RWMutex
	registry   = map[string]registration{}
)

// Register adds a connector type. Connector packages call this from init(), and
// cmd/proxui/connectors.go decides which packages are linked in — that blank
// import list is the entire plugin manifest.
//
// Registering the same type twice is a programming error, so it panics at
// startup rather than silently shadowing an implementation.
func Register(info Info, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()

	if info.Type == "" {
		panic("connector: Register called with an empty type")
	}
	if f == nil {
		panic("connector: Register called with a nil factory for " + info.Type)
	}
	if _, exists := registry[info.Type]; exists {
		panic("connector: duplicate registration for type " + info.Type)
	}
	registry[info.Type] = registration{factory: f, info: info}
}

// New builds a connector of the given type.
func New(platformType string, cfg Config, creds Credentials, opts Options) (Connector, error) {
	registryMu.RLock()
	reg, ok := registry[platformType]
	registryMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("%w: unknown platform type %q", ErrInvalidConfig, platformType)
	}
	return reg.factory(cfg, creds, opts)
}

// Registered lists the available connector types, sorted for stable output in
// the API and UI.
func Registered() []Info {
	registryMu.RLock()
	defer registryMu.RUnlock()

	out := make([]Info, 0, len(registry))
	for _, reg := range registry {
		out = append(out, reg.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// IsRegistered reports whether a platform type is available.
func IsRegistered(platformType string) bool {
	registryMu.RLock()
	defer registryMu.RUnlock()
	_, ok := registry[platformType]
	return ok
}

// resetRegistry clears registrations. Test-only.
func resetRegistry() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = map[string]registration{}
}

// Supports reports whether c declares the capability.
func Supports(c Connector, capability Capability) bool {
	for _, have := range c.Capabilities() {
		if have == capability {
			return true
		}
	}
	return false
}
