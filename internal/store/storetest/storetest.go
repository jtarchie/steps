// Package storetest is the store's conformance suite: the behavior tests
// written against internal/store's contract rather than against any one
// driver, so a second driver is proven by the same tests the first one passes.
//
// A driver runs the whole suite from a single test. What stays behind in the
// driver's own package is what measures that driver — its file footprint, its
// pragmas, its schema stamp — and nothing a caller of store.Store can observe.
package storetest

import (
	"reflect"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// OpenFunc opens a handle on the named pipeline.
//
// It takes a pipeline name because a state database holds SEVERAL pipelines
// and several of these tests are about that: two calls within one test must
// reach the same database, so `open(t, "web")` and `open(t, "infra")` are
// siblings in one file rather than two unrelated stores. Different tests must
// not share one, since they run in parallel.
//
// The suite closes what it opens; a driver that also wants a cleanup close
// registers its own.
type OpenFunc func(t *testing.T, pipeline string) store.Store

// suite carries the opener to every test in it. Each test is an exported
// method so Run can find it by reflection — a conformance test that nobody
// registered is a test that silently stops running, which is the failure this
// package exists to prevent.
type suite struct {
	open OpenFunc
}

// Run runs the conformance suite against one driver.
func Run(t *testing.T, open OpenFunc) {
	t.Helper()

	value := reflect.ValueOf(suite{open: open})
	typ := value.Type()
	ran := 0

	for index := range typ.NumMethod() {
		method := typ.Method(index)
		if !strings.HasPrefix(method.Name, "Test") {
			continue
		}

		test, ok := value.Method(index).Interface().(func(*testing.T))
		if !ok {
			t.Fatalf("%s is %s, want func(*testing.T) — the suite cannot run it, so nobody does", method.Name, method.Type)
		}

		ran++

		t.Run(method.Name, test)
	}

	if ran == 0 {
		t.Fatal("the conformance suite ran no tests")
	}
}

// TestTheOpenerSharesOneDatabase holds the contract on OpenFunc itself.
//
// Several tests below are about two pipelines in ONE database not seeing each
// other's rows. Every one of them passes for free against an opener that hands
// out a separate database per name — the isolation they assert would be the
// filesystem's rather than the query's. This is the one test that fails there,
// so the rest mean what they say.
func (s suite) TestTheOpenerSharesOneDatabase(t *testing.T) {
	t.Parallel()

	web := s.open(t, "web")

	defer func() { _ = web.Close() }()

	infra := s.open(t, "infra")

	defer func() { _ = infra.Close() }()

	if web.Pipeline() != "web" || infra.Pipeline() != "infra" {
		t.Fatalf("the handles are scoped to %q and %q, want web and infra", web.Pipeline(), infra.Pipeline())
	}

	if web.Description() != infra.Description() {
		t.Fatalf("the handles are backed by %q and %q, want one database — Description is stable, so two of them means the scoping tests below assert nothing", web.Description(), infra.Description())
	}
}
