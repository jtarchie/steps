package store

import "context"

// Blobs is the cache index (#259): per step- or resource-cache entry, which tree each name points at and who holds it. internal/workspace declares the same methods as CacheIndex, since its depguard allow-list does not name this package.
type Blobs interface {
	// CacheEntry is an entry's trees by name; empty for an unknown key, which a caller reads as a miss.
	CacheEntry(ctx context.Context, kind CacheKind, key string) (map[string]CachedTree, error)
	// RecordCacheEntry replaces the entry's trees and marks it used, then keeps only the keep most recently used entries of its kind. released are the digests of trees in this machine's store (no holder) that some entry named before and none names after: the only trees the caller may remove, since another state database sharing the store knows of trees this one never named.
	RecordCacheEntry(ctx context.Context, kind CacheKind, key string, trees map[string]CachedTree, keep int) (released []string, err error)
	// TouchCacheEntry marks a hit, so the count bound evicts the least recently used entry rather than the oldest.
	TouchCacheEntry(ctx context.Context, kind CacheKind, key string) error
}

// CacheKind is which cache an entry belongs to; the two are bounded apart.
type CacheKind string

// The two caches the index keeps entries for.
const (
	CacheStep     CacheKind = "step"
	CacheResource CacheKind = "resource"
)

// CachedTree is where one of an entry's trees is. Holder empty is this machine's store; otherwise the docker that holds it, which is never part of the key.
type CachedTree struct {
	// Name is what the holder filed the tree under, empty for the entry's own name.
	Name   string
	Digest string
	Holder string
}
