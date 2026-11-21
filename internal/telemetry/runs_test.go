package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/codes"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/jtarchie/steps/internal/events"
)

func recordRun(t *testing.T, published ...events.Event) map[string]sdktrace.ReadOnlySpan {
	t.Helper()

	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	tracer, err := newRunTracer(provider.Tracer("test"), metricnoop.NewMeterProvider().Meter("test"))
	if err != nil {
		t.Fatal(err)
	}

	at := time.Now()
	ctx := events.WithRunID(context.Background(), "r1")

	for i, event := range published {
		event.RunID = "r1"
		event.At = at.Add(time.Duration(i) * time.Millisecond)

		switch event.Type {
		case events.TypeJobStarted:
			ctx = tracer.StartRun(ctx, event.RunID, event.Job)
		case events.TypeJobFinished:
			tracer.EndRun(ctx, event.RunID, event.Status, event.Text)
		default:
			tracer.Event(ctx, event)
		}
	}

	spans := map[string]sdktrace.ReadOnlySpan{}
	for _, stub := range exporter.GetSpans().Snapshots() {
		spans[stub.Name()] = stub
	}

	return spans
}

func attr(span sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range span.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}

	return ""
}

// A chain skip publishes only its skip, so its span is drawn whole from that one event, under the run.
func TestAChainSkipIsASpanOfItsOwn(t *testing.T) {
	spans := recordRun(t,
		events.Event{Type: events.TypeJobStarted, Job: "build"},
		events.Event{Type: events.TypeStepSkipped, StepID: 1, StepKind: "task", StepName: "unchanged", Status: "skipped", Text: "replayed from cache"},
		events.Event{Type: events.TypeJobFinished, Job: "build", Status: "succeeded"},
	)

	skipped, ok := spans["task unchanged"]
	if !ok {
		t.Fatalf("no span for the skipped step among %v", spans)
	}

	if skipped.Parent().SpanID() != spans["RUN build"].SpanContext().SpanID() {
		t.Error("the skipped step is not under the run")
	}

	if attr(skipped, "cicd.pipeline.task.run.result") != "skip" || attr(skipped, "steps.skip.reason") != "replayed from cache" {
		t.Errorf("the skip does not say so: %v", skipped.Attributes())
	}
}

// A note's text is data, never the event's name: a backend groups by name, and a version or a PR title in it is a new name per run.
func TestANoteIsAnEventWithAFixedName(t *testing.T) {
	spans := recordRun(t,
		events.Event{Type: events.TypeJobStarted, Job: "build"},
		events.Event{Type: events.TypeStepStarted, StepID: 1, StepKind: "get", StepName: "ticket"},
		events.Event{Type: events.TypeStepNote, StepID: 1, Name: events.NoteFact, Detail: "url", Text: "https://x/1"},
		events.Event{Type: events.TypeStepNote, StepID: 1, Status: "info", Text: "get: ticket (version: 7)"},
		events.Event{Type: events.TypeStepFinished, StepID: 1, StepKind: "get", StepName: "ticket", Status: "succeeded"},
		events.Event{Type: events.TypeJobFinished, Job: "build", Status: "succeeded"},
	)

	got := spans["get ticket"].Events()
	if len(got) != 2 || got[0].Name != "steps.note" || got[1].Name != "steps.note" {
		t.Fatalf("span events = %v, want two named steps.note", got)
	}

	if said(got[0], "steps.note.fact") != "url" || said(got[0], "steps.note.text") != "https://x/1" {
		t.Errorf("the fact's event = %v, want its name and value apart", got[0].Attributes)
	}

	if said(got[1], "steps.note.text") != "get: ticket (version: 7)" || said(got[1], "steps.note.level") != "info" || said(got[1], "steps.note.fact") != "" {
		t.Errorf("the note's event = %v, want its text and level and no fact", got[1].Attributes)
	}
}

func said(event sdktrace.Event, key string) string {
	for _, kv := range event.Attributes {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}

	return ""
}

// A failed command's error quotes its script, and the status message is copied onto every span above the step.
func TestAFailedSpansMessageIsBounded(t *testing.T) {
	long := "x" + strings.Repeat("é", 20_000)

	spans := recordRun(t,
		events.Event{Type: events.TypeJobStarted, Job: "build"},
		events.Event{Type: events.TypeStepStarted, StepID: 1, StepKind: "task", StepName: "big"},
		events.Event{Type: events.TypeStepFinished, StepID: 1, StepKind: "task", StepName: "big", Status: "failed", Text: long},
		events.Event{Type: events.TypeJobFinished, Job: "build", Status: "failed", Text: "short"},
	)

	got := spans["task big"].Status().Description
	if len(got) > statusLimit+100 || !strings.HasPrefix(got, "xéé") || !strings.Contains(got, "truncated") || !utf8.ValidString(got) {
		t.Errorf("status message is %d bytes (%.40q…), want it cut to about %d, valid, and saying so", len(got), got, statusLimit)
	}

	if got := spans["RUN build"].Status().Description; got != "short" {
		t.Errorf("a short message became %q", got)
	}
}

// A step its job cut off still ends, under the job's verdict, rather than leaking an open span.
func TestAStepOpenWhenItsJobEndsIsEnded(t *testing.T) {
	spans := recordRun(t,
		events.Event{Type: events.TypeJobStarted, Job: "build"},
		events.Event{Type: events.TypeStepStarted, StepID: 1, StepKind: "task", StepName: "wedged"},
		events.Event{Type: events.TypeJobFinished, Job: "build", Status: "aborted"},
	)

	if _, ok := spans["task wedged"]; !ok {
		t.Fatal("the cut-off step's span never ended")
	}

	if got := attr(spans["RUN build"], "cicd.pipeline.result"); got != "cancellation" {
		t.Errorf("an aborted run's result = %q, want cancellation", got)
	}
}

func TestResultOfMapsEveryFinishStatus(t *testing.T) {
	for _, status := range []string{events.StatusSucceeded, events.StatusFailed, events.StatusErrored, events.StatusAborted, events.StatusSkipped} {
		if got := resultOf(status); got == status {
			t.Errorf("resultOf(%q) fell through to the default; map it to a CI/CD result", status)
		}
	}
}

// A poll's check is a trace of its own: named by its resource alone, carrying the pipeline it was polled for, and an error when the check failed.
func TestACheckIsARootSpanNamedByItsResource(t *testing.T) {
	check := checked(t, errors.New("exit 3"))
	if check.Name() != "CHECK repo" || check.Parent().IsValid() {
		t.Fatalf("span = %q under %v, want a root named CHECK repo", check.Name(), check.Parent())
	}

	if attr(check, "steps.resource") != "repo" || attr(check, "steps.pipeline") != "ci" || len(check.Attributes()) != 2 {
		t.Errorf("attributes = %v, want the resource once and its pipeline", check.Attributes())
	}

	if check.Status().Code != codes.Error || check.Status().Description != "exit 3" {
		t.Errorf("status = %v, want the check's error", check.Status())
	}

	if got := checked(t, nil).Status().Code; got != codes.Unset {
		t.Errorf("a check that passed has status %v", got)
	}
}

// checked is the one span a check of repo, polled for pipeline ci and ending on err, leaves behind.
func checked(t *testing.T, err error) sdktrace.ReadOnlySpan {
	t.Helper()

	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	tracer, newErr := newRunTracer(provider.Tracer("test"), metricnoop.NewMeterProvider().Meter("test"))
	if newErr != nil {
		t.Fatal(newErr)
	}

	ctx, end := tracer.StartCheck(events.WithLogAttrs(context.Background(), slog.String("pipeline", "ci"), slog.String("resource", "repo")), "repo")
	if !trace.SpanContextFromContext(ctx).IsValid() {
		t.Fatal("work under the check has no span to sit under")
	}

	end(err)

	spans := exporter.GetSpans().Snapshots()
	if len(spans) != 1 {
		t.Fatalf("spans = %v, want one", spans)
	}

	return spans[0]
}
