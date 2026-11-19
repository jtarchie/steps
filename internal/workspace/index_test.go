package workspace

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
)

// memIndex is a CacheIndex over a map; the store's drivers are held to the same contract by internal/store/storetest.
type memIndex struct {
	mu      sync.Mutex
	entries map[string]map[string]CachedTree
	used    map[string]int
	clock   int
}

func newMemIndex() *memIndex {
	return &memIndex{entries: map[string]map[string]CachedTree{}, used: map[string]int{}}
}

func (m *memIndex) CacheEntry(_ context.Context, kind, key string) (map[string]CachedTree, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return maps.Clone(m.entries[kind+"/"+key]), nil
}

func (m *memIndex) RecordCacheEntry(_ context.Context, kind, key string, trees map[string]CachedTree, keep int) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	before := m.local()

	m.clock++
	m.entries[kind+"/"+key] = maps.Clone(trees)
	m.used[kind+"/"+key] = m.clock

	ofKind := []string{}

	for id := range m.entries {
		if strings.HasPrefix(id, kind+"/") {
			ofKind = append(ofKind, id)
		}
	}

	slices.SortFunc(ofKind, func(a, b string) int { return m.used[b] - m.used[a] })

	for _, id := range ofKind[min(keep, len(ofKind)):] {
		delete(m.entries, id)
		delete(m.used, id)
	}

	after := m.local()

	var released []string

	for digest := range before {
		if !after[digest] {
			released = append(released, digest)
		}
	}

	return released, nil
}

func (m *memIndex) local() map[string]bool {
	digests := map[string]bool{}

	for _, trees := range m.entries {
		for _, tree := range trees {
			if tree.Holder == "" {
				digests[tree.Digest] = true
			}
		}
	}

	return digests
}

func (m *memIndex) TouchCacheEntry(_ context.Context, kind, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.entries[kind+"/"+key]; ok {
		m.clock++
		m.used[kind+"/"+key] = m.clock
	}

	return nil
}

// indexes is one memIndex per root, standing in for the state database a root's runs share across providers.
var indexes sync.Map //nolint:gochecknoglobals // a fixture registry keyed by each test's own temp root

func indexFor(root string) *memIndex {
	index, _ := indexes.LoadOrStore(root, newMemIndex())

	return index.(*memIndex) //nolint:forcetypeassert // only memIndexes are stored
}

// newTestProvider is NewProvider with the root's index attached when the root is durable, as internal/cli attaches the state database.
func newTestProvider(ws *Settings, keep bool) (Provider, error) {
	provider, err := NewProvider(ws, keep)
	if err != nil || ws == nil || ws.Root == "" {
		return provider, err
	}

	provider.AttachCacheIndex(indexFor(ws.Root))

	return provider, nil
}
