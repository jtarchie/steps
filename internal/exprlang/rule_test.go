package exprlang

import "testing"

func TestAWhenRuleReadsTheCallAndThePerson(t *testing.T) {
	t.Parallel()

	rule, err := CompileWhen(`args.owner in who.Organizations and tool startsWith "gh_"`)
	if err != nil {
		t.Fatal(err)
	}

	member := RuleWho{Organizations: []string{"acme"}}

	for _, tc := range []struct {
		name string
		env  WhenEnv
		want bool
	}{
		{"member, right tool", WhenEnv{Tool: "gh_search", Args: map[string]any{"owner": "acme"}, Who: member}, true},
		{"someone else's org", WhenEnv{Tool: "gh_search", Args: map[string]any{"owner": "evil"}, Who: member}, false},
		{"missing argument", WhenEnv{Tool: "gh_search", Args: map[string]any{}, Who: member}, false},
		{"not a person", WhenEnv{Tool: "gh_search", Args: map[string]any{"owner": "acme"}}, false},
		{"wrong tool", WhenEnv{Tool: "other", Args: map[string]any{"owner": "acme"}, Who: member}, false},
	} {
		got, err := rule(tc.env)
		if err != nil || got.Allowed != tc.want || got.Message != "" {
			t.Errorf("%s: = %+v, %v; want allowed=%v", tc.name, got, err, tc.want)
		}
	}
}

func TestFailRefusesWithAReasonTheCallerReads(t *testing.T) {
	t.Parallel()

	rule, err := CompileWhen(`args.n <= 10 ? true : fail("ask for at most 10, you sent " + string(args.n))`)
	if err != nil {
		t.Fatal(err)
	}

	got, err := rule(WhenEnv{Args: map[string]any{"n": 50}})
	if err != nil || got.Allowed || got.Message != "ask for at most 10, you sent 50" {
		t.Errorf("= %+v, %v", got, err)
	}

	got, err = rule(WhenEnv{Args: map[string]any{"n": 3}})
	if err != nil || !got.Allowed {
		t.Errorf("= %+v, %v; want allowed", got, err)
	}
}

func TestAnAcceptRuleReadsWhatCameBack(t *testing.T) {
	t.Parallel()

	rule, err := CompileAccept(`not (result.text matches "(?i)private key") and result.error == ""`)
	if err != nil {
		t.Fatal(err)
	}

	for text, want := range map[string]bool{"all fine": true, "-----BEGIN PRIVATE KEY-----": false} {
		got, err := rule(AcceptEnv{Result: RuleResult{Text: text}})
		if err != nil || got.Allowed != want {
			t.Errorf("%q = %+v, %v; want %v", text, got, err, want)
		}
	}

	got, _ := rule(AcceptEnv{Result: RuleResult{Error: "boom"}})
	if got.Allowed {
		t.Error("an errored result passed a rule that requires none")
	}
}

func TestARuleCannotReachAnythingButTheCall(t *testing.T) {
	t.Parallel()

	for _, src := range []string{`http("http://x") != nil`, `env("HOME") == ""`, `file("a") == ""`, `result.text == ""`} {
		_, err := CompileWhen(src)
		if err == nil {
			t.Errorf("%s compiled in a when: rule", src)
		}
	}

	for _, src := range []string{`http("http://x") != nil`, `env("HOME") == ""`, `file("a") == ""`} {
		_, err := CompileAccept(src)
		if err == nil {
			t.Errorf("%s compiled in an accept: rule", src)
		}
	}
}

func TestARuleThatAnswersSomethingElseRefusesTheCallInsteadOfPassingIt(t *testing.T) {
	t.Parallel()

	rule, err := CompileWhen(`args.owner`)
	if err != nil {
		t.Fatal(err)
	}

	got, err := rule(WhenEnv{Args: map[string]any{"owner": 3}})
	if err == nil || got.Allowed {
		t.Errorf("a non-boolean answer = %+v, %v; want an error and no pass", got, err)
	}
}
