package e2e

// A Linear GraphQL API in a process, as fakegithub_test.go is GitHub's: just enough of it for the linear-* types, answering by what each query asks for, and recording every write so a test can read back what a put did.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeLinearKey is what every fake answers to, sent bare as a personal key is; anything else is a 401.
const fakeLinearKey = "lin_api_test_key"

type fakeLinearState struct{ ID, Name, Type string }

type fakeLinearComment struct {
	ID, Body, CreatedAt, Parent, Author string
}

type fakeLinearEntry struct {
	CreatedAt string
	ToState   string // a state id
}

type fakeLinearIssue struct {
	Identifier, Title, Description, Team, State, Branch string
	Labels                                              []string // label ids
	CreatedAt                                           string
	History                                             []fakeLinearEntry
	Comments                                            []fakeLinearComment
}

type fakeLinear struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	states   []fakeLinearState
	labels   map[string]string // id → name; "team" scope is the one team, workspace labels start ws-
	issues   []*fakeLinearIssue
	posted   []string // "<identifier>: <body>"
	updates  []map[string]any
	requests []string
	// failNext, when set, answers the next mutation with this status instead.
	failNext int
	nextID   int
}

func newFakeLinear(t *testing.T) *fakeLinear {
	t.Helper()

	fake := &fakeLinear{
		t: t,
		states: []fakeLinearState{
			{"st-backlog", "Backlog", "backlog"}, {"st-todo", "Todo", "unstarted"},
			{"st-prog", "In Progress", "started"}, {"st-done", "Done", "completed"},
		},
		labels: map[string]string{"lb-ready": "agent-ready", "lb-bug": "bug", "ws-urgent": "urgent"},
		nextID: 100,
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)

	return fake
}

func (f *fakeLinear) URL() string { return f.server.URL + "/graphql" }

func (f *fakeLinear) add(issue fakeLinearIssue) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if issue.Team == "" {
		issue.Team = "tm-eng"
	}

	if issue.State == "" {
		issue.State = "st-todo"
	}

	if issue.CreatedAt == "" {
		issue.CreatedAt = "2026-10-01T09:00:00.000Z"
	}

	f.issues = append(f.issues, &issue)
}

// move puts an issue in a state, as a person dragging it on the board does: one history entry, at the time given.
func (f *fakeLinear) move(identifier, state, at string) {
	f.edit(identifier, func(issue *fakeLinearIssue) {
		issue.State = state
		issue.History = append(issue.History, fakeLinearEntry{CreatedAt: at, ToState: state})
	})
}

func (f *fakeLinear) edit(identifier string, change func(*fakeLinearIssue)) {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, issue := range f.issues {
		if issue.Identifier == identifier {
			change(issue)
		}
	}
}

func (f *fakeLinear) stateOf(identifier string) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, issue := range f.issues {
		if issue.Identifier == identifier {
			return issue.State
		}
	}

	return ""
}

func (f *fakeLinear) labelNames(identifier string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	var names []string

	for _, issue := range f.issues {
		if issue.Identifier == identifier {
			for _, id := range issue.Labels {
				names = append(names, f.labels[id])
			}
		}
	}

	return names
}

func (f *fakeLinear) postedComments() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.posted)
}

func (f *fakeLinear) updateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.updates)
}

func (f *fakeLinear) lastFilter() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()

	var last map[string]any

	for _, request := range f.requests {
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}

		if json.Unmarshal([]byte(request), &body) == nil && strings.Contains(body.Query, "issues(filter") {
			last, _ = body.Variables["filter"].(map[string]any)
		}
	}

	return last
}

func (f *fakeLinear) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != fakeLinearKey {
		http.Error(w, `{"errors":[{"message":"Authentication required"}]}`, http.StatusUnauthorized)

		return
	}

	var body struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}

	raw := new(strings.Builder)
	decoder := json.NewDecoder(r.Body)

	var whole json.RawMessage

	err := decoder.Decode(&whole)
	if err != nil || json.Unmarshal(whole, &body) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)

		return
	}

	raw.Write(whole)

	f.mu.Lock()
	defer f.mu.Unlock()

	f.requests = append(f.requests, raw.String())

	if strings.Contains(body.Query, "mutation") && f.failNext != 0 {
		status := f.failNext
		f.failNext = 0
		w.WriteHeader(status)

		return
	}

	data, problem := f.answer(body.Query, body.Variables)

	w.Header().Set("Content-Type", "application/json")

	if problem != "" {
		f.reply(w, map[string]any{"errors": []map[string]any{{"message": problem}}})

		return
	}

	f.reply(w, map[string]any{"data": data})
}

func (f *fakeLinear) reply(w http.ResponseWriter, body map[string]any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		f.t.Errorf("fake Linear: %v", err)

		return
	}

	_, _ = w.Write(encoded)
}

func (f *fakeLinear) answer(query string, vars map[string]any) (map[string]any, string) {
	switch {
	case strings.Contains(query, "commentCreate"):
		return f.createComment(vars)
	case strings.Contains(query, "issueUpdate"):
		return f.updateIssue(vars)
	case strings.Contains(query, "teams(filter"):
		return f.teams(vars), ""
	case strings.Contains(query, "issueLabels("):
		return f.labelNodes(), ""
	case strings.Contains(query, "issues(filter"):
		return f.listIssues(vars), ""
	case strings.Contains(query, "comments(first"):
		return f.commentsOf(vars), ""
	case strings.Contains(query, "branchName"):
		return f.issueDoc(vars), ""
	case strings.Contains(query, "issue(id"):
		return f.issueRef(vars), ""
	}

	return nil, "fake Linear: unhandled query " + query
}

func (f *fakeLinear) teams(vars map[string]any) map[string]any {
	asked, _ := vars["team"].(string)
	if asked != "ENG" && asked != "Engineering" {
		return map[string]any{"teams": map[string]any{"nodes": []any{}}}
	}

	return map[string]any{"teams": map[string]any{"nodes": []any{map[string]any{
		"id": "tm-eng", "key": "ENG", "name": "Engineering", "states": map[string]any{"nodes": f.stateNodes()},
	}}}}
}

func (f *fakeLinear) stateNodes() []map[string]any {
	states := make([]map[string]any, len(f.states))
	for i, state := range f.states {
		states[i] = map[string]any{"id": state.ID, "name": state.Name, "type": state.Type}
	}

	return states
}

func (f *fakeLinear) labelNodes() map[string]any {
	nodes := make([]map[string]any, 0, len(f.labels))

	for id, name := range f.labels {
		nodes = append(nodes, map[string]any{"id": id, "name": name})
	}

	return map[string]any{"issueLabels": map[string]any{"nodes": nodes}}
}

func (f *fakeLinear) stateByID(id string) fakeLinearState {
	for _, state := range f.states {
		if state.ID == id {
			return state
		}
	}

	return fakeLinearState{}
}

// matches applies the filter the finder sends: the team, a state by id or type, every label of the and: list.
func (f *fakeLinear) matches(issue *fakeLinearIssue, filter map[string]any) bool {
	if team, _ := filter["team"].(map[string]any); team != nil && eqOf(team["id"]) != issue.Team {
		return false
	}

	if state, _ := filter["state"].(map[string]any); state != nil {
		if ids, ok := inOf(state["id"]); ok && !slices.Contains(ids, issue.State) {
			return false
		}

		if types, ok := inOf(state["type"]); ok && !slices.Contains(types, f.stateByID(issue.State).Type) {
			return false
		}
	}

	all, _ := filter["and"].([]any)
	for _, clause := range all {
		wanted := eqOf(clause.(map[string]any)["labels"].(map[string]any)["some"].(map[string]any)["id"])
		if !slices.Contains(issue.Labels, wanted) {
			return false
		}
	}

	return true
}

func eqOf(node any) string {
	mapping, _ := node.(map[string]any)
	value, _ := mapping["eq"].(string)

	return value
}

func inOf(node any) ([]string, bool) {
	mapping, _ := node.(map[string]any)

	values, ok := mapping["in"].([]any)
	if !ok {
		return nil, false
	}

	out := make([]string, len(values))
	for i, value := range values {
		out[i], _ = value.(string)
	}

	return out, true
}

func (f *fakeLinear) listIssues(vars map[string]any) map[string]any {
	filter, _ := vars["filter"].(map[string]any)
	depth, _ := vars["history"].(float64)
	withHistory, _ := vars["withHistory"].(bool)

	nodes := []map[string]any{}

	// Newest first, as Linear answers: the finder must not depend on the order.
	for i := len(f.issues) - 1; i >= 0; i-- {
		issue := f.issues[i]
		if !f.matches(issue, filter) {
			continue
		}

		history := []map[string]any{}

		for j := len(issue.History) - 1; j >= 0 && float64(len(history)) < depth; j-- {
			state := f.stateByID(issue.History[j].ToState)
			history = append(history, map[string]any{"createdAt": issue.History[j].CreatedAt, "toState": map[string]any{"id": state.ID, "type": state.Type}})
		}

		node := map[string]any{"identifier": issue.Identifier, "createdAt": issue.CreatedAt}
		if withHistory {
			node["history"] = map[string]any{"nodes": history}
		}

		nodes = append(nodes, node)
	}

	return map[string]any{"issues": map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}, "nodes": nodes}}
}

func (f *fakeLinear) find(vars map[string]any) *fakeLinearIssue {
	id, _ := vars["id"].(string)

	for _, issue := range f.issues {
		if issue.Identifier == id || "uuid-"+issue.Identifier == id {
			return issue
		}
	}

	return nil
}

func (f *fakeLinear) issueDoc(vars map[string]any) map[string]any {
	issue := f.find(vars)
	if issue == nil {
		return map[string]any{"issue": nil}
	}

	state := f.stateByID(issue.State)
	labels := []map[string]any{}

	for _, id := range issue.Labels {
		labels = append(labels, map[string]any{"id": id, "name": f.labels[id]})
	}

	return map[string]any{"issue": map[string]any{
		"id": "uuid-" + issue.Identifier, "identifier": issue.Identifier, "title": issue.Title, "description": issue.Description,
		"url": "https://linear.example/acme/issue/" + issue.Identifier, "branchName": issue.Branch,
		"state":  map[string]any{"id": state.ID, "name": state.Name, "type": state.Type},
		"team":   map[string]any{"id": "tm-eng", "key": "ENG", "name": "Engineering"},
		"labels": map[string]any{"nodes": labels},
	}}
}

func (f *fakeLinear) issueRef(vars map[string]any) map[string]any {
	issue := f.find(vars)
	if issue == nil {
		return map[string]any{"issue": nil}
	}

	return map[string]any{"issue": map[string]any{
		"id": "uuid-" + issue.Identifier, "identifier": issue.Identifier, "url": "https://linear.example/acme/issue/" + issue.Identifier,
		"team": map[string]any{"id": issue.Team, "key": "ENG", "name": "Engineering", "states": map[string]any{"nodes": f.stateNodes()}},
	}}
}

func (f *fakeLinear) commentsOf(vars map[string]any) map[string]any {
	issue := f.find(vars)
	if issue == nil {
		return map[string]any{"issue": nil}
	}

	nodes := []map[string]any{}

	// Newest first, as Linear answers.
	for i := len(issue.Comments) - 1; i >= 0; i-- {
		comment := issue.Comments[i]
		node := map[string]any{"id": comment.ID, "body": comment.Body, "createdAt": comment.CreatedAt, "user": map[string]any{"name": comment.Author}, "parent": nil}

		if comment.Parent != "" {
			node["parent"] = map[string]any{"id": comment.Parent}
		}

		nodes = append(nodes, node)
	}

	return map[string]any{"issue": map[string]any{"comments": map[string]any{"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""}, "nodes": nodes}}}
}

func (f *fakeLinear) createComment(vars map[string]any) (map[string]any, string) {
	input, _ := vars["input"].(map[string]any)
	issueID, _ := input["issueId"].(string)
	body, _ := input["body"].(string)

	issue := f.find(map[string]any{"id": issueID})
	if issue == nil {
		return nil, "Entity not found: Issue"
	}

	f.nextID++
	id := "cm-" + string(rune('a'+f.nextID%26)) + issue.Identifier
	f.posted = append(f.posted, issue.Identifier+": "+body)

	return map[string]any{"commentCreate": map[string]any{"success": true, "comment": map[string]any{"id": id, "url": "https://linear.example/acme/issue/" + issue.Identifier + "#comment-" + id}}}, ""
}

func (f *fakeLinear) updateIssue(vars map[string]any) (map[string]any, string) {
	issue := f.find(vars)
	if issue == nil {
		return nil, "Entity not found: Issue"
	}

	input, _ := vars["input"].(map[string]any)
	f.updates = append(f.updates, input)

	if state, ok := input["stateId"].(string); ok && state != issue.State {
		issue.State = state
		issue.History = append(issue.History, fakeLinearEntry{CreatedAt: "2026-10-05T09:00:00.000Z", ToState: state})
	}

	if removed, ok := input["removedLabelIds"].([]any); ok {
		for _, id := range removed {
			issue.Labels = slices.DeleteFunc(issue.Labels, func(label string) bool { return label == id })
		}
	}

	if added, ok := input["addedLabelIds"].([]any); ok {
		for _, id := range added {
			if label, _ := id.(string); !slices.Contains(issue.Labels, label) {
				issue.Labels = append(issue.Labels, label)
			}
		}
	}

	return map[string]any{"issueUpdate": map[string]any{"success": true, "issue": map[string]any{"id": "uuid-" + issue.Identifier, "identifier": issue.Identifier, "url": "https://linear.example/acme/issue/" + issue.Identifier, "state": map[string]any{"name": f.stateByID(issue.State).Name}}}}, ""
}
