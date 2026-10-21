package storetest

// credentials: sealed bytes held per pipeline, replaced in place, scoped like every other row, and gone with the pipeline.

import (
	"bytes"
	"context"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

func (s suite) TestCredentialsReplaceAndStayInTheirPipeline(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	app := s.open(t, "app")
	other := s.open(t, "other")

	_, found, err := app.Credential(ctx, "mcp:tracker")
	if err != nil || found {
		t.Fatalf("Credential before any save = found %v, %v; want none", found, err)
	}

	for _, sealed := range [][]byte{{1, 2, 3}, {4, 5}} {
		err = app.SaveCredential(ctx, "mcp:tracker", sealed)
		if err != nil {
			t.Fatal(err)
		}
	}

	got, found, err := app.Credential(ctx, "mcp:tracker")
	if err != nil || !found || !bytes.Equal(got, []byte{4, 5}) {
		t.Fatalf("Credential after two saves = %v, %v, %v; want the second", got, found, err)
	}

	theOtherPipelineSeesNone(ctx, t, other, app.CredentialScope())
}

func theOtherPipelineSeesNone(ctx context.Context, t *testing.T, other store.Store, appScope string) {
	t.Helper()

	_, found, err := other.Credential(ctx, "mcp:tracker")
	if err != nil || found {
		t.Errorf("another pipeline read app's credential: found %v, %v", found, err)
	}

	if appScope == other.CredentialScope() {
		t.Errorf("two pipelines share credential scope %q: a row moved between them would open", appScope)
	}
}

func (s suite) TestARenameKeepsCredentialsAndADestroyTakesThem(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	st := s.open(t, "before")
	scope := st.CredentialScope()

	err := st.SaveCredential(ctx, "mcp:tracker", []byte{9})
	if err != nil {
		t.Fatal(err)
	}

	err = st.Rename(ctx, "after")
	if err != nil {
		t.Fatal(err)
	}

	renamed := s.open(t, "after")
	if renamed.CredentialScope() != scope {
		t.Errorf("a rename changed the credential scope %q to %q: every sealed value would stop opening", scope, renamed.CredentialScope())
	}

	_, found, err := renamed.Credential(ctx, "mcp:tracker")
	if err != nil || !found {
		t.Fatalf("credential after a rename: found %v, %v", found, err)
	}

	err = renamed.Delete(ctx)
	if err != nil {
		t.Fatal(err)
	}

	again := s.open(t, "after")
	if again.CredentialScope() == scope {
		t.Error("a destroyed pipeline's scope was handed to its successor: a restored row would open under it")
	}

	_, found, err = again.Credential(ctx, "mcp:tracker")
	if err != nil || found {
		t.Errorf("a pipeline created under a destroyed one's name inherited its credential: found %v, %v", found, err)
	}
}

// The backstop for a write racing a destroy: a handle opened before the destroy still holds the old id, and the row it would write names a pipeline that is gone.
func (s suite) TestACredentialForADestroyedPipelineIsRefused(t *testing.T) {
	t.Parallel()

	ctx := ctxFor(t)
	before := s.open(t, "racing")
	destroyer := s.open(t, "racing")

	err := destroyer.Delete(ctx)
	if err != nil {
		t.Fatal(err)
	}

	err = before.SaveCredential(ctx, "mcp:tracker", []byte{1})
	if err == nil {
		t.Fatal("a credential was saved for a pipeline already destroyed")
	}
}
