package main

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/jtarchie/steps/internal/config"

	"golang.org/x/tools/go/analysis/analysistest"
)

// TestAnalyzer pins both halves: a tagless switch whose cases read a
// `kind:`-tagged field is reported (compound cases, mixed with modifiers,
// through embedding), and one that reads no kind field — or reads one only in
// a case body — is not, nor is one carrying a reasoned ignore directive.
func TestAnalyzer(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "dispatch")
}

// The pass reports only when a selector resolves to a kind-tagged field, so the tags must be exactly the fields Step.Kind dispatches on: a kind whose field went untagged is dispatch this pass cannot see, and a count floor let that through.
func TestKindTagsAreExactlyTheKindFields(t *testing.T) {
	typ := reflect.TypeFor[config.Step]()
	kinds := map[config.StepKind]string{}

	for field := range typ.Fields() {
		if !field.IsExported() || field.Type.Kind() == reflect.Struct {
			continue
		}

		var step config.Step

		setNonZero(reflect.ValueOf(&step).Elem().FieldByIndex(field.Index))

		kind, isKind := step.Kind()
		if problem := tagProblem(field, kind, isKind); problem != "" {
			t.Errorf("config.Step.%s %s", field.Name, problem)
		}

		if isKind {
			kinds[kind] = field.Name
		}
	}

	if len(kinds) == 0 {
		t.Fatal("no field of config.Step makes a kind; the walk has stopped seeing the table, and an empty walk passes everything")
	}
}

// tagProblem is how a field's kind tag disagrees with the kind setting it alone makes, or "".
func tagProblem(field reflect.StructField, kind config.StepKind, isKind bool) string {
	tag, tagged := field.Tag.Lookup(kindTag)

	switch {
	case isKind && !tagged:
		return fmt.Sprintf("alone makes a step %q but carries no `%s:` tag, so a tagless switch over it goes unreported", kind, kindTag)
	case !isKind && tagged:
		return fmt.Sprintf("is tagged `%s:%q` but setting it makes no kind; no stepKinds row tests it", kindTag, tag)
	case isKind && string(kind) != tag:
		return fmt.Sprintf("is tagged `%s:%q` but makes a step %q", kindTag, tag, kind)
	}

	return ""
}

// setNonZero gives a field the smallest value a kind's isSet could notice.
func setNonZero(v reflect.Value) {
	//nolint:exhaustive // a kind field is a string, a pointer or a slice; anything else stays zero and so selects no kind
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 1, 1))
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64:
		v.SetInt(1)
	}
}
