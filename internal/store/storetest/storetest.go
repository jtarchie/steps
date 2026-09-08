// Package storetest is the store's conformance suite: the behavior tests
// written against internal/store's contract rather than against any one
// driver, so a second driver is proven by the same tests the first one passes.
//
// A driver runs the whole suite from a single test. What stays behind in the
// driver's own package is what measures that driver — its file footprint, its
// pragmas, its schema stamp — and nothing a caller of store.Store can observe.
package storetest

import (
	"context"
	"reflect"
	"slices"
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

	// Closed here, once, rather than by each test: a handle a test forgot
	// leaks a connection past the test's own cleanup, and nothing in this
	// package would notice.
	closing := func(t *testing.T, pipeline string) store.Store {
		t.Helper()

		st := open(t, pipeline)
		t.Cleanup(func() { _ = st.Close() })

		return st
	}

	// A pointer, so a method with either receiver is in the set: reflected
	// over a value, a pointer-receiver test compiles and is never run.
	value := reflect.ValueOf(&suite{open: closing})
	typ := value.Type()
	ran := 0

	for index := range typ.NumMethod() {
		method := typ.Method(index)
		if !strings.HasPrefix(method.Name, "Test") {
			t.Fatalf("%s is exported but is not a Test, so nobody would run it", method.Name)
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
	infra := s.open(t, "infra")

	if web.Pipeline() != "web" || infra.Pipeline() != "infra" {
		t.Fatalf("the handles are scoped to %q and %q, want web and infra", web.Pipeline(), infra.Pipeline())
	}

	if web.Description() != infra.Description() {
		t.Fatalf("the handles are backed by %q and %q, want one database — Description is stable, so two of them means the scoping tests below assert nothing", web.Description(), infra.Description())
	}

	// The description is a promise; this is the storage. Two private
	// databases can answer to one description (`:memory:` does), and the
	// scoping tests would then assert nothing.
	rows, err := web.Reader().Pipelines(context.Background())
	if err != nil {
		t.Fatalf("Pipelines through web: %v", err)
	}

	names := make([]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, row.Name)
	}

	if !slices.Contains(names, "infra") {
		t.Fatalf("web's database holds pipelines %q, want infra among them — the descriptions matched but the storage did not", names)
	}
}
