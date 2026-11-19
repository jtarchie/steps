package ssmdial

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

type startOnlyAPI struct {
	API

	stream string

	mu         sync.Mutex
	terminated []string
	cancelled  bool
}

func (f *startOnlyAPI) StartSession(context.Context, *ssm.StartSessionInput, ...func(*ssm.Options)) (*ssm.StartSessionOutput, error) {
	return &ssm.StartSessionOutput{SessionId: aws.String("s-orphan"), StreamUrl: aws.String(f.stream), TokenValue: aws.String("t")}, nil
}

func (f *startOnlyAPI) TerminateSession(ctx context.Context, in *ssm.TerminateSessionInput, _ ...func(*ssm.Options)) (*ssm.TerminateSessionOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.terminated = append(f.terminated, aws.ToString(in.SessionId))
	f.cancelled = ctx.Err() != nil

	return &ssm.TerminateSessionOutput{}, nil
}

// A session SSM started whose stream never opened still counts against the account's session limit until it is terminated.
func TestForwardTerminatesASessionWhoseStreamFailedToOpen(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	fake := &startOnlyAPI{stream: "ws" + strings.TrimPrefix(server.URL, "http")}

	_, err := Forward(t.Context(), fake, "i-0abc", 22)
	if err == nil {
		t.Fatal("Forward succeeded against a stream that refused the upgrade")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.terminated) != 1 || fake.terminated[0] != "s-orphan" {
		t.Errorf("terminated %v, want the started session s-orphan", fake.terminated)
	}
}

// The likeliest reason Open fails is the step being aborted, so the termination must not ride the cancelled context.
func TestForwardTerminatesUnderItsOwnContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	fake := &startOnlyAPI{stream: "ws://127.0.0.1:1/never"}

	_, err := Forward(ctx, fake, "i-0abc", 22)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Forward = %v, want the abort", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if len(fake.terminated) != 1 || fake.cancelled {
		t.Errorf("terminated %v (under a cancelled context: %v), want s-orphan under a live one", fake.terminated, fake.cancelled)
	}
}
