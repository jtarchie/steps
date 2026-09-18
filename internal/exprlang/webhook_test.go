package exprlang

import "testing"

// TestAWebhookFilterMustBeBoolean: refused when the pipeline is set where the type is known, and at the delivery where it depends on the payload.
func TestAWebhookFilterMustBeBoolean(t *testing.T) {
	_, err := Webhook(`event`, true)
	if err == nil {
		t.Error("a string filter compiled")
	}

	run, err := Webhook(`payload.ref`, true)
	if err != nil {
		t.Fatal(err)
	}

	_, err = run(WebhookEnv{Payload: map[string]any{"ref": "main"}})
	if err == nil {
		t.Error("a filter that evaluated to a string ran")
	}
}

// TestAWebhookExpressionCannotReachOut: evaluation is pure, so env(), http(), file(), fail() and the clock are not there to call.
func TestAWebhookExpressionCannotReachOut(t *testing.T) {
	for _, src := range []string{`env("HOME")`, `http("GET", "http://example.com")`, `file("x")`, `fail("no")`, `string(now())`} {
		_, err := Webhook(src, false)
		if err == nil {
			t.Errorf("%s compiled; a webhook expression must not reach outside the delivery", src)
		}
	}
}

func TestAWebhookExpressionReadsTheDelivery(t *testing.T) {
	run, err := Webhook(`event + ":" + headers["x-a"] + ":" + payload.after + ":" + query.q`, false)
	if err != nil {
		t.Fatal(err)
	}

	got, err := run(WebhookEnv{Event: "push", Headers: map[string]string{"x-a": "h"}, Query: map[string]string{"q": "v"}, Payload: map[string]any{"after": "sha"}})
	if err != nil {
		t.Fatal(err)
	}

	if got != "push:h:sha:v" {
		t.Errorf("got %v", got)
	}
}
