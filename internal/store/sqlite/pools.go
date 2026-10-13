package sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
)

// pools is the writer and reader connections every handle on one file shares
// inside this process.
//
// A daemon holds a handle per pipeline on one file, and each used to open
// pools of its own: N pipelines were N writers on one file, serialized by
// sqlite's sleep-and-retry busy handler instead of a queue. At 100 pipelines
// a wait outran busy_timeout and polls, claims and run events failed
// "database is locked" — the one writer openDSN keeps per file only holds if
// a file has one pool.
type pools struct {
	db      *sql.DB
	reads   *sql.DB
	key     string
	holders int
}

var (
	poolsMu     sync.Mutex
	poolsByFile = map[string]*pools{}
)

// acquirePools hands out the file's pools, opening them for the first holder.
func acquirePools(path string) (*pools, error) {
	key, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("could not resolve state db %q: %w", path, err)
	}

	poolsMu.Lock()
	defer poolsMu.Unlock()

	if held, ok := poolsByFile[key]; ok {
		held.holders++

		return held, nil
	}

	db, err := openDB(path)
	if err != nil {
		return nil, err
	}

	reads, err := openReadPool(path)
	if err != nil {
		_ = db.Close()

		return nil, err
	}

	held := &pools{db: db, reads: reads, key: key, holders: 1}
	poolsByFile[key] = held

	return held, nil
}

// release drops one holder and reports whether it was the last, in which case
// the pools are the caller's to close and no later open is handed them.
func (p *pools) release() bool {
	poolsMu.Lock()
	defer poolsMu.Unlock()

	p.holders--
	if p.holders > 0 {
		return false
	}

	delete(poolsByFile, p.key)

	return true
}
