package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

var olderLink = regexp.MustCompile(`<a href="([^"]+)">older</a>`)

// TestTheApprovalsHistoryPages: past one page the history offers the next by cursor, and the next page holds what the first could not, without the waiting cards again.
func TestTheApprovalsHistoryPages(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	for i := range decisionsPage + 1 {
		id, err := pipeline.Store.RequestApproval(ctx, "deploy", fmt.Sprintf("ship build %03d?", i))
		if err != nil {
			t.Fatalf("RequestApproval: %v", err)
		}

		err = pipeline.Store.DecideApproval(ctx, id, store.ApprovalApproved, "alice", "")
		if err != nil {
			t.Fatalf("DecideApproval: %v", err)
		}
	}

	_, err := pipeline.Store.RequestApproval(ctx, "deploy", "still waiting?")
	if err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}

	_, first := get(t, server, "/p/demo/approvals")
	if strings.Contains(first, "ship build 000?") {
		t.Error("the first page holds more than a page")
	}

	match := olderLink.FindStringSubmatch(first)
	if match == nil {
		t.Fatal("a history longer than a page offers no older link")
	}

	code, second := get(t, server, html.UnescapeString(match[1]))
	if code != http.StatusOK {
		t.Fatalf("GET %s = %d", match[1], code)
	}

	if !strings.Contains(second, "ship build 000?") {
		t.Error("the older page does not hold the oldest decision")
	}

	if strings.Contains(second, "still waiting?") {
		t.Error("the older page draws the waiting cards again")
	}
}

// TestADecisionPageNarrowsToTheIDANoticeLinksTo: ?id= is what an on_pending's pending.url opens, waiting or decided.
func TestADecisionPageNarrowsToTheIDANoticeLinksTo(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	request := func(message string) int64 {
		t.Helper()

		id, err := pipeline.Store.RequestApproval(ctx, "deploy", message)
		if err != nil {
			t.Fatalf("RequestApproval: %v", err)
		}

		return id
	}

	decided := request("ship the old one?")
	waiting := request("ship this one?")
	request("ship another?")

	err := pipeline.Store.DecideApproval(ctx, decided, store.ApprovalApproved, "alice", "")
	if err != nil {
		t.Fatalf("DecideApproval: %v", err)
	}

	for id, want := range map[int64]string{waiting: "ship this one?", decided: "ship the old one?"} {
		path := fmt.Sprintf("/p/demo/approvals?id=%d", id)

		_, body := get(t, server, path)
		if !strings.Contains(body, want) {
			t.Errorf("%s is missing %q", path, want)
		}

		for _, other := range []string{"ship the old one?", "ship this one?", "ship another?"} {
			if other != want && strings.Contains(body, other) {
				t.Errorf("%s still shows %q", path, other)
			}
		}
	}
}

// TestTheQuestionsPageNarrows: the chips and the box narrow both what waits and what was answered.
func TestTheQuestionsPageNarrows(t *testing.T) {
	t.Parallel()

	server, pipeline := testPipeline(t)
	ctx := context.Background()

	err := pipeline.Store.StartRun(ctx, store.NewRun{ID: "run-1", JobName: "build", Workspace: "/tmp/ws"})
	if err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	ask := func(text string) int64 {
		t.Helper()

		question, _, err := pipeline.Store.AskQuestion(ctx, store.Question{RunID: "run-1", JobName: "build", AgentName: "writer", Question: text})
		if err != nil {
			t.Fatalf("AskQuestion: %v", err)
		}

		return question.ID
	}

	answered := ask("Which region?")
	ask("Ship today?")

	err = pipeline.Store.AnswerQuestion(ctx, answered, "us-east-2", "alice")
	if err != nil {
		t.Fatalf("AnswerQuestion: %v", err)
	}

	for _, tc := range []struct {
		query        string
		want, refuse []string
	}{
		{"", []string{"Which region?", "Ship today?", `href="/p/demo/runs/run-1"`, "us-east-2"}, nil},
		{"?status=answered", []string{"Which region?"}, []string{"Ship today?"}},
		{"?status=pending", []string{"Ship today?"}, []string{"Which region?"}},
		{"?q=US-EAST", []string{"Which region?"}, []string{"Ship today?"}},
		{"?job=deploy", []string{"Nothing matches."}, []string{"Which region?", "Ship today?"}},
	} {
		_, body := get(t, server, "/p/demo/questions"+tc.query)

		for _, want := range tc.want {
			if !strings.Contains(body, want) {
				t.Errorf("questions%s is missing %q", tc.query, want)
			}
		}

		for _, refuse := range tc.refuse {
			if strings.Contains(body, refuse) {
				t.Errorf("questions%s still shows %q", tc.query, refuse)
			}
		}
	}
}
