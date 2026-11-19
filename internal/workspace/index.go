package workspace

import "context"

// CacheIndex is internal/store's Blobs facet in this package's own types, which internal/cli adapts: which tree each step- and resource-cache entry names, machine-wide (see the schema comment on cache_entries).
type CacheIndex interface {
	CacheEntry(ctx context.Context, kind, key string) (map[string]CachedTree, error)
	// RecordCacheEntry answers the store trees no entry names any more: the only ones this machine's store may remove, since another state database sharing the root knows of trees this one never named.
	RecordCacheEntry(ctx context.Context, kind, key string, trees map[string]CachedTree, keep int) (released []string, err error)
	TouchCacheEntry(ctx context.Context, kind, key string) error
}

// CachedTree is where one of an entry's trees is: an empty Holder is this machine's store, otherwise the docker that holds it.
type CachedTree = heldRecord

const (
	kindStep     = "step"
	kindResource = "resource"
)

func (p *isolatingProvider) AttachCacheIndex(index CacheIndex) bool {
	if p.store == nil {
		return false
	}

	p.cache = &resourceCache{backend: p.backend, store: p.store, index: index, maxEntries: p.resourceCacheMax}
	p.stepCache = &stepCache{backend: p.backend, store: p.store, index: index, maxEntries: defaultStepCacheMaxEntries}

	return true
}
