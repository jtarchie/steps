package secret

import (
	"strings"
	"testing"
)

// The segment rule must follow every way a template can read the job or resource, not just a literal .job.
func TestLookupSegmentRuleCatchesEveryTemplateSpelling(t *testing.T) {
	t.Parallel()

	templates := []struct {
		tmpl string
		job  bool
	}{
		{`{{ .pipeline }}/{{ .job }}/{{ .name }}`, true},
		{`{{ .pipeline }}/{{ index . "job" }}/{{ .name }}`, true},
		{`{{ .pipeline }}/{{ $j := .job }}{{ $j }}/{{ .name }}`, true},
		{`{{ .pipeline }}/{{ .resource }}/{{ .name }}`, false},
		{`{{ .pipeline }}/{{ index . "resource" }}/{{ .name }}`, false},
		{`{{ .pipeline }}/{{ $r := .resource }}{{ $r }}/{{ .name }}`, false},
	}

	for _, entry := range templates {
		resolver := resolverFor(t, opSource(fakeOp(t, map[string]string{"prod/x": "prod"}), "\n    - path: '"+entry.tmpl+"'\n"))

		for _, bad := range []string{"..", "a/b", "../../prod", "."} {
			ref := Ref{Pipeline: "dev", Job: "j", Resource: "r", Source: "ci", Name: "x"}
			if entry.job {
				ref.Job = bad
			} else {
				ref.Resource = bad
			}

			_, err := resolver.Lookup(t.Context(), ref)
			if err == nil || !strings.Contains(err.Error(), "cannot be used in a lookup path") {
				t.Errorf("%s with job %q resource %q: Lookup = %v, want refused", entry.tmpl, ref.Job, ref.Resource, err)
			}
		}
	}
}

func TestLookupAllowsLegitimateNamesInEverySpelling(t *testing.T) {
	t.Parallel()

	resolver := resolverFor(t, opSource(fakeOp(t, map[string]string{"dev/build-1.x_y/x": "ok"}), `
    - path: '{{ .pipeline }}/{{ index . "job" }}/{{ .name }}'
`))

	value, err := resolver.Lookup(t.Context(), Ref{Pipeline: "dev", Job: "build-1.x_y", Source: "ci", Name: "x"})
	if err != nil || value != "ok" {
		t.Errorf("Lookup = %q, %v", value, err)
	}
}

func TestLookupRefusesAHostileName(t *testing.T) {
	t.Parallel()

	resolver := resolverFor(t, opSource(fakeOp(t, map[string]string{"dev/x": "dev"}), `
    - path: '{{ .pipeline }}/{{ .name }}'
`))

	for _, name := range []string{"", "..", "a/../b", "/abs", "a//b", "a\x00b"} {
		_, err := resolver.Lookup(t.Context(), Ref{Pipeline: "dev", Source: "ci", Name: name})
		if err == nil || !strings.Contains(err.Error(), "secret name") {
			t.Errorf("name %q: Lookup = %v, want refused", name, err)
		}
	}
}
