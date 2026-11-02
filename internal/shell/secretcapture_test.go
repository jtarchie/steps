package shell

import (
	"strings"
	"testing"

	"github.com/jtarchie/steps/internal/redact"
)

// A value straddling the capture limit was cut before it was masked, so the head of it survived; masking in front of the capture means no fragment does.
func TestCaptureMasksAValueCutAtTheLimit(t *testing.T) {
	t.Parallel()

	const secret = "straddling-secret-value-0123456789"

	secrets := &redact.Secrets{}
	secrets.Add(secret)

	runner, err := NewRunner(RunnerSpec{Cwd: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	ctx := redact.WithSecrets(t.Context(), secrets)

	// 90 bytes of filler then the value: a 100-byte limit cuts it ten bytes in.
	stdout, _, _, err := RunCaptureFullLimitedStreamed(ctx, runner, `printf '%090d' 0; printf '%s and more' '`+secret+`'`, 100, "")
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(stdout, secret[:10]) {
		t.Errorf("the truncated capture holds a fragment of the secret: %q", stdout)
	}
}
