package postgres

import (
	"runtime"
	"sync"
)

// sharedPoolSize bounds the connections one URL holds in this process, every
// pipeline's handle sharing them. Each handle used to open four of its own, so
// a daemon serving 25 pipelines met Postgres's default max_connections of 100.
// Wide enough that pipelines waiting on each other's advisory locks leave
// connections for everyone else; no transaction here takes a second
// connection while it holds one, so a full pool queues rather than deadlocks.
var sharedPoolSize = max(8, 2*runtime.GOMAXPROCS(0))

// pool is the connection every writable handle on one URL shares.
type pool struct {
	connection

	key     string
	holders int
}

var (
	poolsMu    sync.Mutex
	poolsByURL = map[string]*pool{}
)

// acquirePool hands out the URL's pool, opening it for the first holder.
func acquirePool(rawURL string) (*pool, error) {
	poolsMu.Lock()
	defer poolsMu.Unlock()

	if held, ok := poolsByURL[rawURL]; ok {
		held.holders++

		return held, nil
	}

	conn, err := open(rawURL, false)
	if err != nil {
		return nil, err
	}

	conn.db.SetMaxOpenConns(sharedPoolSize)

	held := &pool{connection: conn, key: rawURL, holders: 1}
	poolsByURL[rawURL] = held

	return held, nil
}

// release drops one holder and reports whether it was the last, in which case
// the pool is the caller's to close and no later open is handed it.
func (p *pool) release() bool {
	poolsMu.Lock()
	defer poolsMu.Unlock()

	p.holders--
	if p.holders > 0 {
		return false
	}

	delete(poolsByURL, p.key)

	return true
}
