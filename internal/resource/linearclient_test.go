package resource

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

func linearTestClient(t *testing.T, key string, handler http.HandlerFunc) *linearClient {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	t.Setenv("RESOURCE_TEST_LINEAR_KEY", key)

	client, err := newLinearClient(context.Background(), config.LinearConnection{TokenEnv: "RESOURCE_TEST_LINEAR_KEY", Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}

	return client
}

// TestLinearClientSendsAKeyAsLinearExpectsIt: a personal key goes bare in Authorization, and any other token as Bearer, whatever it starts with.
func TestLinearClientSendsAKeyAsLinearExpectsIt(t *testing.T) {
	for key, want := range map[string]string{"lin_api_abc": "lin_api_abc", "lin_oauth_abc": "Bearer lin_oauth_abc", "abc123": "Bearer abc123"} {
		client := linearTestClient(t, key, func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != want {
				t.Errorf("Authorization = %q, want %q", got, want)
			}

			_, _ = w.Write([]byte(`{"data":{}}`))
		})

		err := client.query(context.Background(), "query { viewer { id } }", nil, nil)
		if err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}
}

func TestLinearUnsetKeyIsNamed(t *testing.T) {
	t.Setenv("RESOURCE_TEST_LINEAR_KEY", "")

	_, err := newLinearClient(context.Background(), config.LinearConnection{TokenEnv: "RESOURCE_TEST_LINEAR_KEY"})
	if err == nil || !strings.Contains(err.Error(), "$RESOURCE_TEST_LINEAR_KEY is not set") {
		t.Fatalf("newLinearClient = %v, want it to name the variable", err)
	}
}

// TestLinearClientFailsOnErrorsInA200: GraphQL answers 200 with an errors array, and that is a failed step with Linear's own words, not an empty result.
func TestLinearClientFailsOnErrorsInA200(t *testing.T) {
	client := linearTestClient(t, "k", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"message":"Entity not found: Issue"},{"message":"second"}]}`))
	})

	var out struct{}

	err := client.query(context.Background(), "query { x }", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "Entity not found: Issue; second") {
		t.Fatalf("query = %v, want Linear's messages", err)
	}
}

func TestLinearClientRetriesAThrottledQueryAndANeverSentMutation(t *testing.T) {
	noBackoff(t)

	for _, tc := range []struct {
		name   string
		status int
		write  bool
		calls  int32
		fails  bool
	}{
		{"a throttled query is sent again", http.StatusTooManyRequests, false, 3, false},
		{"a throttled mutation is sent again", http.StatusTooManyRequests, true, 3, false},
		{"an unavailable query is sent again", http.StatusServiceUnavailable, false, 3, false},
		{"an unavailable mutation is not: it may have been applied", http.StatusServiceUnavailable, true, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32

			client := linearTestClient(t, "k", func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) < 3 {
					w.WriteHeader(tc.status)

					return
				}

				_, _ = w.Write([]byte(`{"data":{}}`))
			})

			var err error

			if tc.write {
				err = client.mutate(context.Background(), "mutation { x }", nil, nil)
			} else {
				err = client.query(context.Background(), "query { x }", nil, nil)
			}

			if (err != nil) != tc.fails {
				t.Errorf("err = %v, want failure %v", err, tc.fails)
			}

			if calls.Load() != tc.calls && !tc.fails {
				t.Errorf("sent %d requests, want %d", calls.Load(), tc.calls)
			}

			if tc.fails && calls.Load() != 1 {
				t.Errorf("a write was sent %d times, want once", calls.Load())
			}
		})
	}
}

func TestLinearEnteredAtIsTheLatestMoveIntoAFilterState(t *testing.T) {
	t.Parallel()

	node := linearIssueNode{CreatedAt: "2026-10-01T09:00:00.000Z"}

	from := &linearState{ID: "backlog", Type: "backlog"}

	push := func(at, state, kind string) {
		node.History.Nodes = append(node.History.Nodes, linearHistoryEntry{CreatedAt: at, FromState: from, ToState: &linearState{ID: state, Type: kind}})
		from = &linearState{ID: state, Type: kind}
	}

	if got := enteredAt(node, []string{"todo"}, nil); got != node.CreatedAt {
		t.Errorf("no history: %q, want the creation time", got)
	}

	push("2026-10-02T00:00:00.000Z", "todo", "unstarted")
	push("2026-10-03T00:00:00.000Z", "prog", "started")
	push("2026-10-04T00:00:00.000Z", "todo", "unstarted")
	push("2026-10-05T00:00:00.000Z", "prog", "started")

	if got := enteredAt(node, []string{"todo"}, nil); got != "2026-10-04T00:00:00.000Z" {
		t.Errorf("by id: %q, want the last move into Todo, not the last move at all", got)
	}

	if got := enteredAt(node, nil, []string{"started"}); got != "2026-10-05T00:00:00.000Z" {
		t.Errorf("by type: %q, want the last move into a started state", got)
	}

	push("2026-10-06T00:00:00.000Z", "review", "started")
	node.History.Nodes = append(node.History.Nodes, linearHistoryEntry{CreatedAt: "2026-10-07T00:00:00.000Z"})

	if got := enteredAt(node, nil, []string{"started"}); got != "2026-10-05T00:00:00.000Z" {
		t.Errorf("between two started states: %q, want the move in from outside, not the move within", got)
	}

	if got := enteredAt(node, []string{"prog", "review"}, nil); got != "2026-10-05T00:00:00.000Z" {
		t.Errorf("between two named states: %q, want the move in from outside, not the move within", got)
	}

	created := linearIssueNode{CreatedAt: "2026-10-01T09:00:00.000Z"}
	created.History.Nodes = []linearHistoryEntry{{CreatedAt: "2026-10-01T09:00:01.000Z", ToState: &linearState{ID: "todo", Type: "unstarted"}}}

	if got := enteredAt(created, []string{"todo"}, nil); got != "2026-10-01T09:00:01.000Z" {
		t.Errorf("no origin: %q, want it counted as entering from outside", got)
	}
}

// TestLinearLabelsArePagedToTheEnd: a label on a later page is found, not refused as missing.
func TestLinearLabelsArePagedToTheEnd(t *testing.T) {
	client := linearTestClient(t, "k", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Variables map[string]any `json:"variables"`
		}

		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)

		if body.Variables["after"] == nil {
			_, _ = w.Write([]byte(`{"data":{"issueLabels":{"pageInfo":{"hasNextPage":true,"endCursor":"c1"},"nodes":[{"id":"l1","name":"bug"}]}}}`))

			return
		}

		_, _ = w.Write([]byte(`{"data":{"issueLabels":{"pageInfo":{"hasNextPage":false,"endCursor":"c2"},"nodes":[{"id":"l2","name":"agent-ready"}]}}}`))
	})

	labels, err := client.labelsOf(context.Background(), "tm")
	if err != nil {
		t.Fatal(err)
	}

	ids, err := labelIDs("source.labels", labels, []string{"bug", "agent-ready"})
	if err != nil || strings.Join(ids, ",") != "l1,l2" {
		t.Fatalf("labelIDs = %v, %v; want both pages' labels", ids, err)
	}
}

// TestLinearUpdateStateNeedsNoTeamRequest: the issue's own lookup carries its team's states, so a state move resolves without asking for the team again.
func TestLinearUpdateStateNeedsNoTeamRequest(t *testing.T) {
	var calls atomic.Int32

	client := linearTestClient(t, "k", func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	})

	issue := linearIssueRef{ID: "uuid", Identifier: "ENG-1"}
	issue.Team.Key = "ENG"
	issue.Team.States.Nodes = []linearState{{ID: "st-todo", Name: "Todo"}, {ID: "st-prog", Name: "In Progress"}}

	input, err := client.updateInput(context.Background(), issue, config.LinearUpdateParams{State: "in progress"})
	if err != nil {
		t.Fatal(err)
	}

	if input["stateId"] != "st-prog" || calls.Load() != 0 {
		t.Fatalf("input = %v after %d requests, want st-prog after none", input, calls.Load())
	}
}

// TestLinearUpdateLabelErrorsInFieldOrder: with both sides naming a missing label, the error names add_labels every time.
func TestLinearUpdateLabelErrorsInFieldOrder(t *testing.T) {
	client := linearTestClient(t, "k", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"issueLabels":{"nodes":[]}}}`))
	})

	for range 20 {
		_, err := client.updateInput(context.Background(), linearIssueRef{}, config.LinearUpdateParams{AddLabels: []string{"a"}, RemoveLabels: []string{"b"}})
		if err == nil || !strings.Contains(err.Error(), "params.add_labels") {
			t.Fatalf("updateInput = %v, want params.add_labels named first", err)
		}
	}
}

// TestLinearHistoryIsAskedForOnlyWithAStateFilter: a source with no states reads no history, so the check does not depend on Linear accepting history(first: 0).
func TestLinearHistoryIsAskedForOnlyWithAStateFilter(t *testing.T) {
	for _, withHistory := range []bool{false, true} {
		var sent map[string]any

		client := linearTestClient(t, "lin_api_k", func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}

			err := json.NewDecoder(r.Body).Decode(&body)
			if err != nil {
				t.Error(err)
			}

			if !strings.Contains(body.Query, "@include(if: $withHistory)") {
				t.Errorf("query does not gate history: %s", body.Query)
			}

			sent = body.Variables

			_, _ = w.Write([]byte(`{"data":{"issues":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}`))
		})

		_, err := client.issues(context.Background(), map[string]any{}, withHistory)
		if err != nil {
			t.Fatal(err)
		}

		if sent["withHistory"] != withHistory || sent["history"] == float64(0) {
			t.Errorf("withHistory=%v: sent %v, want withHistory %v and a positive depth", withHistory, sent, withHistory)
		}
	}
}
