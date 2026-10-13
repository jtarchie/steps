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
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/steps/internal/store"
)

// Every listing takes a limit, and the limit means the same thing on each:
// zero is no limit, and anything else is honoured. Each facet's file pins its
// own listings (the HonoursItsLimit tests), because the one test that once
// asserted a listing bound went with the method it bounded, and nothing else
// in the tree noticed a LIMIT clause disappear.

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

func hashOf(n int) string { return fmt.Sprintf("%064x", n) }

// ctxFor is the test's context, named so the calls above read as one line.
func ctxFor(t *testing.T) context.Context {
	t.Helper()

	return t.Context()
}

// assertStamped holds a driver to the time it stamped on a write: a UTC
// instant between the moments either side of that write, at the resolution
// the column keeps — which may be the whole second. Non-zero is not enough: a
// driver that parsed the wrong layout, or read another column, is non-zero.
func assertStamped(t *testing.T, what string, got, before, after time.Time) {
	t.Helper()

	if got.Location() != time.UTC || got.Before(before.Truncate(time.Second)) || got.After(after) {
		t.Errorf("%s = %v, want a UTC time between %v and %v", what, got, before, after)
	}
}
