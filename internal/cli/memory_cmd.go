package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/jtarchie/steps/internal/store"
)

// MemoryCmd is what agent steps keep about people between runs — readable and
// deletable by whoever runs the pipeline, not only by the model that wrote it.
type MemoryCmd struct {
	List MemoryListCmd `cmd:"" default:"withargs"                        help:"list memory scopes, or one scope's entries with --scope"`
	Rm   MemoryRmCmd   `cmd:"" help:"delete entries from a memory scope"`
}

// MemoryListCmd lists scopes, or one scope's entries.
type MemoryListCmd struct {
	ReadFlags `embed:""`
	Scope     string `help:"list this scope's entries instead of every scope" name:"scope"`
}

// Run prints the listing.
func (m *MemoryListCmd) Run() error {
	if nothingRecorded(m.ReadFlags, "no memories are kept") {
		return nil
	}

	st, cleanup, err := openStore(m.ReadFlags)
	if err != nil {
		return err
	}
	defer cleanup()

	if m.Scope == "" {
		return printMemoryScopes(st)
	}

	return printMemories(st, m.Scope)
}

func printMemoryScopes(st store.Memories) error {
	scopes, err := st.MemoryScopes(context.Background())
	if err != nil {
		return fmt.Errorf("could not list memory scopes: %w", err)
	}

	if len(scopes) == 0 {
		fmt.Println("no memories are kept")

		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	_, _ = fmt.Fprintln(writer, "SCOPE\tENTRIES\tLAST REMEMBERED")

	for _, scope := range scopes {
		_, _ = fmt.Fprintf(writer, "%s\t%d\t%s\n", scope.Scope, scope.Entries, scope.LastAt)
	}

	return flushTable(writer, "memory scopes")
}

func printMemories(st store.Memories, scope string) error {
	memories, err := st.ListMemories(context.Background(), scope, 0)
	if err != nil {
		return fmt.Errorf("could not list memories: %w", err)
	}

	if len(memories) == 0 {
		fmt.Printf("nothing is remembered in scope %s\n", scope)

		return nil
	}

	writer := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	_, _ = fmt.Fprintln(writer, "ID\tREMEMBERED\tRUN\tTEXT")

	for _, memory := range memories {
		run := memory.RunID
		if run == "" {
			run = "-"
		}

		_, _ = fmt.Fprintf(writer, "%d\t%s\t%s\t%s\n", memory.ID, memory.CreatedAt, run, strconv.Quote(memory.Text))
	}

	return flushTable(writer, "memories")
}

func flushTable(writer *tabwriter.Writer, what string) error {
	err := writer.Flush()
	if err != nil {
		return fmt.Errorf("could not write the %s table: %w", what, err)
	}

	return nil
}

// MemoryRmCmd deletes named entries of one scope, or all of it.
type MemoryRmCmd struct {
	ReadFlags `embed:""`
	Scope     string  `help:"the scope to delete from"                                     name:"scope"                                required:""`
	All       bool    `help:"delete every entry of the scope — everything kept about it"   name:"all"`
	IDs       []int64 `arg:""                                                              help:"entry ids, from steps memory --scope" optional:""`
}

// Run deletes what was named.
func (m *MemoryRmCmd) Run() error {
	if m.All == (len(m.IDs) > 0) {
		return errors.New("name entry ids, or pass --all for the whole scope: steps memory rm --scope <scope> <id>... -p <pipeline>")
	}

	st, cleanup, err := openStore(m.ReadFlags)
	if err != nil {
		return err
	}
	defer cleanup()

	return forgetMemories(st, m.Scope, m.All, m.IDs)
}

func forgetMemories(st store.Memories, scope string, all bool, ids []int64) error {
	ctx := context.Background()

	if all {
		gone, err := st.ForgetScope(ctx, scope)
		if err != nil {
			return fmt.Errorf("could not forget scope %s: %w", scope, err)
		}

		fmt.Printf("forgot %d entries of scope %s\n", gone, scope)

		return nil
	}

	for _, id := range ids {
		forgot, err := st.Forget(ctx, scope, id)
		if err != nil {
			return fmt.Errorf("could not forget memory %d: %w", id, err)
		}

		if !forgot {
			return fmt.Errorf("memory %d is not in scope %s — steps memory --scope %s lists what is", id, scope, scope)
		}

		fmt.Printf("forgot memory %d\n", id)
	}

	return nil
}
