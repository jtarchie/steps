package pipeline

import (
	"context"
	"strings"
	"testing"
)

func TestWithWorkersReportsFirstBadTagSorted(t *testing.T) {
	t.Parallel()

	mappings := map[string]string{}
	for _, tag := range []string{"t09", "t03", "t07", "t01", "t05", "t08", "t02", "t06", "t04", "t00"} {
		mappings[tag] = "not-a-worker-url"
	}

	_, err := WithWorkers(context.Background(), mappings)
	if err == nil || !strings.Contains(err.Error(), "--worker t00:") {
		t.Fatalf("want the sorted-first tag t00 reported, got %v", err)
	}
}
