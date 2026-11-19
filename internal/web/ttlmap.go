package web

import (
	"container/list"
	"sync"
	"time"
)

// maxPending bounds every in-flight sign-in, consent and code: the routes that mint them answer anonymous callers, so the map is what a flood fills.
const maxPending = 1000

// ttlMap is a single-use, expiring, size-bounded map. Expired entries are only swept when it is full, so an insert is O(1) until then; at the cap the oldest entry goes whoever minted it, so maxPending inserts from anybody empty it of everything else. An entry put with an owner (putFrom) is also held to that owner's own cap, which is what keeps one source's flood off everybody else's entries.
type ttlMap[V any] struct {
	mu      sync.Mutex
	entries map[string]*list.Element
	order   list.List
	// owned counts each owner's live entries.
	owned map[string]int
}

type ttlEntry[V any] struct {
	key     string
	owner   string
	value   V
	expires time.Time
}

func (m *ttlMap[V]) put(key string, value V, expires time.Time) {
	m.putFrom("", 0, key, value, expires)
}

// putFrom is put for an entry owner minted: past ownerCap of its own, it displaces its own oldest rather than anybody else's.
func (m *ttlMap[V]) putFrom(owner string, ownerCap int, key string, value V, expires time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.entries == nil {
		m.entries = map[string]*list.Element{}
		m.owned = map[string]int{}
	}

	if old, ok := m.entries[key]; ok {
		m.removeLocked(old)
	}

	for owner != "" && m.owned[owner] >= ownerCap {
		oldest := m.oldestOfLocked(owner)
		if oldest == nil {
			break
		}

		m.removeLocked(oldest)
	}

	if len(m.entries) >= maxPending {
		m.sweepLocked()
	}

	for len(m.entries) >= maxPending {
		m.removeLocked(m.order.Front())
	}

	m.entries[key] = m.order.PushBack(&ttlEntry[V]{key: key, owner: owner, value: value, expires: expires})

	if owner != "" {
		m.owned[owner]++
	}
}

// oldestOfLocked is owner's oldest entry, nil when it has none.
func (m *ttlMap[V]) oldestOfLocked(owner string) *list.Element {
	for el := m.order.Front(); el != nil; el = el.Next() {
		if entry, _ := el.Value.(*ttlEntry[V]); entry != nil && entry.owner == owner {
			return el
		}
	}

	return nil
}

func (m *ttlMap[V]) sweepLocked() {
	now := time.Now()

	for el := m.order.Front(); el != nil; {
		next := el.Next()
		if entry, _ := el.Value.(*ttlEntry[V]); entry != nil && now.After(entry.expires) {
			m.removeLocked(el)
		}

		el = next
	}
}

func (m *ttlMap[V]) removeLocked(el *list.Element) {
	if entry, _ := el.Value.(*ttlEntry[V]); entry != nil {
		delete(m.entries, entry.key)

		if entry.owner != "" {
			m.owned[entry.owner]--
			if m.owned[entry.owner] <= 0 {
				delete(m.owned, entry.owner)
			}
		}
	}

	m.order.Remove(el)
}

// take is single-use: an entry answered once cannot be answered again, and an expired one is gone without being returned.
func (m *ttlMap[V]) take(key string) (V, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var zero V

	el, ok := m.entries[key]
	if !ok {
		return zero, false
	}

	entry, _ := el.Value.(*ttlEntry[V])
	m.removeLocked(el)

	if entry == nil || !time.Now().Before(entry.expires) {
		return zero, false
	}

	return entry.value, true
}

func (m *ttlMap[V]) len() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.entries)
}
