package e2e

// The fake Linear each linear= doc example runs against. The reader sees api.linear.app; the test sees fakelinear_test.go, reached through the same source.endpoint: seam a self-hosted proxy would use.

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/docs"
	"gopkg.in/yaml.v3"
)

type docLinearFixture struct {
	issues []fakeLinearIssue
	check  func(t *testing.T, fake *fakeLinear)
}

func docLinearIssue() fakeLinearIssue {
	return fakeLinearIssue{
		Identifier: "ENG-12", Title: "Plan the cache", Description: "It should key on content.", Branch: "eng-12-plan-the-cache",
		Labels: []string{"lb-ready"},
		Comments: []fakeLinearComment{
			{ID: "c1", Body: "Use the digest.", CreatedAt: "2026-10-01T09:30:00.000Z", Author: "Bob Builder"},
			{ID: "c2", Body: "Agreed.", CreatedAt: "2026-10-01T09:35:00.000Z", Author: "Alice Planner"},
		},
	}
}

var docLinearFixtures = map[string]docLinearFixture{ //nolint:gochecknoglobals // read-only fixture table, like docGitHubFixtures
	"claim": {
		issues: []fakeLinearIssue{docLinearIssue()},
		check: func(t *testing.T, fake *fakeLinear) {
			t.Helper()

			if got := fake.stateOf("ENG-12"); got != "st-prog" {
				t.Errorf("ENG-12 is in state %q, want In Progress", got)
			}

			if got := strings.Join(fake.labelNames("ENG-12"), ","); got != "urgent" {
				t.Errorf("labels on ENG-12 = %q, want urgent alone", got)
			}

			posted := fake.postedComments()
			if len(posted) != 1 || !strings.HasPrefix(posted[0], "ENG-12: planning ENG-12: Plan the cache, 2 comment(s)") {
				t.Errorf("posted %q, want the plan on ENG-12", posted)
			}
		},
	},
	"types": {issues: []fakeLinearIssue{docLinearIssue()}},
	"acceptance": {
		issues: []fakeLinearIssue{docLinearIssue()},
		check: func(t *testing.T, fake *fakeLinear) {
			t.Helper()

			posted := fake.postedComments()
			if len(posted) != 2 || posted[0] != "ENG-12: Shipped in 4.2\n" || posted[1] != posted[0] {
				t.Errorf("posted %q, want two comments on ENG-12", posted)
			}
		},
	},
}

func (fixture docLinearFixture) serve(t *testing.T) *fakeLinear {
	t.Helper()

	fake := newFakeLinear(t)
	for _, issue := range fixture.issues {
		fake.add(issue)
	}

	return fake
}

// injectDocLinearFixture starts the linear= fixture a block's fence names and points every linear-* resource at it, leaving body untouched when the fence names none.
func injectDocLinearFixture(t *testing.T, block docs.Block, body string) (string, *fakeLinear) {
	t.Helper()

	id := block.LinearID()
	if id == "" {
		return body, nil
	}

	fixture, ok := docLinearFixtures[id]
	if !ok {
		t.Fatalf("fence names linear=%s but docs_linear_test.go has no such fixture", id)
	}

	fake := fixture.serve(t)

	var doc map[string]any

	err := yaml.Unmarshal([]byte(body), &doc)
	if err != nil {
		t.Fatalf("block is not valid YAML: %v", err)
	}

	pointed := 0

	eachOf(doc, "resources", func(entry any) {
		resource, _ := entry.(map[string]any)
		kind, _ := resource["type"].(string)

		if !strings.HasPrefix(kind, "linear-") {
			return
		}

		source, _ := resource["source"].(map[string]any)
		if source == nil {
			source = map[string]any{}
			resource["source"] = source
		}

		source["endpoint"] = fake.URL()
		pointed++
	})

	if pointed == 0 {
		t.Fatalf("the fence names linear=%s, but the block declares no linear-* resource", id)
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	return string(out), fake
}
