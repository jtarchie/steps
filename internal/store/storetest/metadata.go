package storetest

// resource_versions.metadata: what a resource said about a version when a get
// fetched it or a put published it.

import (
	"fmt"
	"testing"

	"github.com/jtarchie/steps/internal/store"
)

// TestVersionMetadataIsKeptWithTheVersion: a get or put's metadata is read
// back beside its version — by the pages, by a job's green versions, and by a
// fetch the cache reused — and an answer with none never erases what a
// version already has.
func (s suite) TestVersionMetadataIsKeptWithTheVersion(t *testing.T) {
	t.Parallel()

	st := s.open(t, "test")
	other := s.open(t, "other")
	ctx := ctxFor(t)

	recordN(t, st, "items", "1")

	first, err := store.EncodeVersion(map[string]any{"n": "1"})
	if err != nil {
		t.Fatal(err)
	}

	unpolled, err := store.EncodeVersion(map[string]any{"n": "7"})
	if err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct {
		version  store.VersionKey
		metadata string
	}{
		{first, `[{"name":"url","value":"old"}]`},
		{first, `[{"name":"url","value":"new"}]`},
		{first, ""},
		{unpolled, `[{"name":"url","value":"seven"}]`},
	} {
		err = st.RecordVersionMetadata(ctx, "items", step.version, step.metadata)
		if err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.VersionMetadata(ctx, "items", []store.VersionKey{first, unpolled})
	if err != nil {
		t.Fatal(err)
	}

	want := map[store.VersionKey]string{first: `[{"name":"url","value":"new"}]`, unpolled: `[{"name":"url","value":"seven"}]`}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("VersionMetadata = %v, want %v", got, want)
	}

	elsewhere, err := other.VersionMetadata(ctx, "items", []store.VersionKey{first, unpolled})
	if err != nil || len(elsewhere) != 0 {
		t.Errorf("another pipeline read this one's metadata: %v, %v", elsewhere, err)
	}

	assertPassedCarries(t, st, first, want[first])
}

// assertPassedCarries: a job's green version is read with its metadata, which is how the job page shows it.
func assertPassedCarries(t *testing.T, st store.Store, version store.VersionKey, metadata string) {
	t.Helper()

	err := st.RecordPassedVersion(ctxFor(t), store.Pass{JobName: "build", Resource: "items", Version: version, BuildID: "b1"})
	if err != nil {
		t.Fatal(err)
	}

	passed, err := st.PassedVersions(ctxFor(t), "build", 10)
	if err != nil || len(passed) != 1 || passed[0].Metadata != metadata {
		t.Errorf("PassedVersions = %+v, %v; want the version's metadata beside it", passed, err)
	}
}
