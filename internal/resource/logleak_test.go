package resource

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/config"
)

// Not t.Parallel(): it swaps the process-wide slog default, and parallel tests are paused until the serial ones finish.
func TestDebugLogsCarryNoSourceParamsOrOutputValues(t *testing.T) {
	var buf bytes.Buffer

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	const (
		srcSecret = "SRC-sentinel-9f3" //nolint:gosec // a sentinel, not a credential
		prmSecret = "PRM-sentinel-4c1" //nolint:gosec // a sentinel, not a credential
		outSecret = "OUT-sentinel-77a"
	)

	ctx := context.Background()
	source := map[string]any{"token": srcSecret}
	params := map[string]any{"note": prmSecret}

	shell := config.ResourceType{Name: "sh", Config: config.ResourceTypeConfig{Check: `printf 'not json ` + outSecret + `'`}}

	_, err := CheckVersions(ctx, Call{Type: shell, Resource: config.Resource{Source: source}})
	if err == nil {
		t.Fatal("shell CheckVersions: want a parse error, the output is the unparsable line that gets logged")
	}

	ex := exprType(`[{v: source.token}]`, `{"f": params.note}`, `{v: "static"}`)

	_, errCheck := CheckVersions(ctx, Call{Type: ex, Resource: config.Resource{Source: source}})
	errIn := RunIn(ctx, Call{Type: ex, Resource: config.Resource{Source: source}, Params: params, Dir: t.TempDir()})
	_, errOut := RunOut(ctx, Call{Type: ex, Resource: config.Resource{Source: source}, Params: params, Dir: t.TempDir()})

	err = errors.Join(errCheck, errIn, errOut)
	if err != nil {
		t.Fatalf("expr backend: %v", err)
	}

	// shell.exec and template.render (other packages) log the rendered command and its output; this pins the resource package's own lines.
	var lines []string

	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(line, "msg=resource.") {
			lines = append(lines, line)
		}
	}

	logged := strings.Join(lines, "\n")
	if !strings.Contains(logged, "resource.check") || !strings.Contains(logged, "source_keys") {
		t.Fatalf("expected debug lines are missing, test is vacuous: %s", logged)
	}

	for _, secret := range []string{srcSecret, prmSecret, outSecret} {
		if strings.Contains(logged, secret) {
			t.Errorf("%q leaked into the debug log: %s", secret, logged)
		}
	}
}
