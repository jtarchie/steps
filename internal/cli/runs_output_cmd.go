package cli

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/jtarchie/steps/internal/events"
	"github.com/jtarchie/steps/internal/store"
)

// RunsOutputCmd prints a step's output whole: the run page links it, and a script or an agent reading a failed gate has no browser.
type RunsOutputCmd struct {
	ReadFlags `embed:""`
	RunID     string `arg:""   help:"the run the step belongs to"`
	Step      string `arg:""   help:"the step's name; every output it recorded is printed, oldest first"`
}

// outputBatch is how many events one read asks for, as a follow's is.
const outputBatch = 500

// Run prints every output the named step recorded in the run.
func (r *RunsOutputCmd) Run() error {
	return withRecorded(r.ReadFlags, func(ctx context.Context, st store.Store) error {
		return r.print(ctx, st)
	})
}

func (r *RunsOutputCmd) print(ctx context.Context, st interface {
	store.Meta
	store.Events
},
) error {
	outputs, names, err := r.recorded(ctx, st)
	if err != nil {
		return err
	}

	if len(names) == 0 {
		return fmt.Errorf("run %q of pipeline %q recorded no step output", r.RunID, st.Pipeline())
	}

	if len(outputs) == 0 {
		return fmt.Errorf("no step %q recorded output in run %q; these did: %s", r.Step, r.RunID, strings.Join(names, ", "))
	}

	for _, output := range outputs {
		err := printOutput(ctx, st, output)
		if err != nil {
			return err
		}
	}

	return nil
}

// recorded is the named step's output events, and every step that has one, for the refusal to name.
func (r *RunsOutputCmd) recorded(ctx context.Context, st store.Events) ([]store.RunEventRow, []string, error) {
	var (
		outputs []store.RunEventRow
		names   []string
		after   int64
	)

	for {
		rows, err := st.RunEvents(ctx, r.RunID, after, outputBatch)
		if err != nil {
			return nil, nil, fmt.Errorf("could not read run %q: %w", r.RunID, err)
		}

		for _, row := range rows {
			after = row.Seq

			if row.Type != events.TypeStepOutput {
				continue
			}

			if !slices.Contains(names, row.StepName) {
				names = append(names, row.StepName)
			}

			if row.StepName == r.Step {
				outputs = append(outputs, row)
			}
		}

		if len(rows) < outputBatch {
			return outputs, names, nil
		}
	}
}

// printOutput writes the stored whole when the record elided it, and the record otherwise: a reaped or refused store leaves only the record.
func printOutput(ctx context.Context, st store.Events, output store.RunEventRow) error {
	n, err := strconv.Atoi(output.Detail)
	if err == nil {
		found, readErr := st.StepOutput(ctx, output.RunID, output.StepID, n, func(chunk []byte) error {
			_, writeErr := os.Stdout.Write(chunk)

			return writeErr //nolint:wrapcheck // StepOutput names the output being read
		})
		if readErr != nil || found {
			return readErr //nolint:wrapcheck // StepOutput names the output being read
		}
	}

	if output.Text == "" {
		return nil
	}

	_, err = fmt.Fprintln(os.Stdout, output.Text)
	if err != nil {
		return fmt.Errorf("could not write the output of %q: %w", output.StepName, err)
	}

	return nil
}
