package e2e

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jtarchie/steps/docs"
)

// docDelivery is a captured request a deliver=<id> fence pre-loads, the way test=<id> picks an LLM scenario: the page shows the pipeline, and this is what a sender would have POSTed to it.
type docDelivery struct {
	resource string
	request  string
}

var docDeliveries = map[string]docDelivery{
	"github-push": {
		resource: "push",
		request: "POST /p/app/hooks/push HTTP/1.1\n" +
			"Content-Type: application/json\n" +
			"X-GitHub-Event: push\n" +
			"X-GitHub-Delivery: 72d3162e-cc78-11e3-81ab-4c9367dc0958\n" +
			"\n" +
			`{"ref":"refs/heads/main","after":"6113728f27ae82c7b1a177c8d03f9e96e0adf246","repository":{"clone_url":"https://github.com/me/app.git"}}`,
	},
	"custom-signed": {
		resource: "deploys",
		request: "POST /p/app/hooks/deploys HTTP/1.1\n" +
			"Content-Type: application/json\n" +
			"X-Timestamp: 1700000000\n" +
			"X-Signature: v1=unchecked-locally\n" +
			"\n" +
			`{"service":"api","status":"finished"}`,
	},
}

// deliveryFlags writes a block's delivery beside its pipeline and returns the --deliver flag that records it, or nothing for a block with no deliver=.
func deliveryFlags(t *testing.T, dir string, block docs.Block) []string {
	t.Helper()

	id := block.DeliverID()
	if id == "" {
		return nil
	}

	delivery, ok := docDeliveries[id]
	if !ok {
		t.Fatalf("fence names deliver=%s but docs_deliveries_test.go has no such delivery", id)
	}

	path := filepath.Join(dir, id+".http")

	err := os.WriteFile(path, []byte(delivery.request), 0o600)
	if err != nil {
		t.Fatal(err)
	}

	return []string{"--deliver", delivery.resource + "=" + path}
}
