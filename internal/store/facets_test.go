package store

import (
	"reflect"
	"sort"
	"testing"
)

// facets is every group Store is composed of, by name.
//
// The list is written out rather than derived because deriving it from Store
// is exactly what the tests below must not do: a method added straight to
// Store, belonging to no aggregate, is the failure they exist to catch, and a
// derived list would adopt it silently.
var facets = map[string]reflect.Type{
	"Meta":       reflect.TypeOf((*Meta)(nil)).Elem(),
	"Runs":       reflect.TypeOf((*Runs)(nil)).Elem(),
	"Cache":      reflect.TypeOf((*Cache)(nil)).Elem(),
	"Blobs":      reflect.TypeOf((*Blobs)(nil)).Elem(),
	"Versions":   reflect.TypeOf((*Versions)(nil)).Elem(),
	"Queue":      reflect.TypeOf((*Queue)(nil)).Elem(),
	"Approvals":  reflect.TypeOf((*Approvals)(nil)).Elem(),
	"Questions":  reflect.TypeOf((*Questions)(nil)).Elem(),
	"Placements": reflect.TypeOf((*Placements)(nil)).Elem(),
	"Usage":      reflect.TypeOf((*Usage)(nil)).Elem(),
	"Events":     reflect.TypeOf((*Events)(nil)).Elem(),
	"Revisions":  reflect.TypeOf((*Revisions)(nil)).Elem(),
	"Control":    reflect.TypeOf((*Control)(nil)).Elem(),
	"Pruning":    reflect.TypeOf((*Pruning)(nil)).Elem(),
}

func methodNames(iface reflect.Type) []string {
	names := make([]string, 0, iface.NumMethod())
	for i := range iface.NumMethod() {
		names = append(names, iface.Method(i).Name)
	}

	sort.Strings(names)

	return names
}

// TestStoreIsExactlyItsFacets is the whole point of the grouping: Store says
// nothing the facets do not. A method declared directly on Store belongs to no
// aggregate, so no consumer can ask for it by name and a second driver has no
// facet to work through — which is how a wide interface grows back.
func TestStoreIsExactlyItsFacets(t *testing.T) {
	t.Parallel()

	inFacet := map[string]string{}

	for name, iface := range facets {
		for _, method := range methodNames(iface) {
			inFacet[method] = name
		}
	}

	storeType := reflect.TypeOf((*Store)(nil)).Elem()

	seen := map[string]bool{}

	for _, method := range methodNames(storeType) {
		seen[method] = true

		if _, ok := inFacet[method]; !ok {
			t.Errorf("Store.%s belongs to no facet", method)
		}
	}

	for method, facet := range inFacet {
		if !seen[method] {
			t.Errorf("%s.%s is in no facet Store embeds", facet, method)
		}
	}
}

// TestFacetsAreDisjoint holds the facets to the file boundaries they were cut
// along: one method is one aggregate's, so a consumer that names a facet takes
// on that aggregate and nothing else. Two facets sharing a method would mean a
// consumer could not tell which part of the database it had declared.
func TestFacetsAreDisjoint(t *testing.T) {
	t.Parallel()

	owner := map[string]string{}

	for _, name := range sortedFacetNames() {
		for _, method := range methodNames(facets[name]) {
			if other, ok := owner[method]; ok {
				t.Errorf("%s is in both %s and %s", method, other, name)

				continue
			}

			owner[method] = name
		}
	}
}

func sortedFacetNames() []string {
	names := make([]string, 0, len(facets))
	for name := range facets {
		names = append(names, name)
	}

	sort.Strings(names)

	return names
}
