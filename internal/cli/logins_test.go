package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	stepsmcp "github.com/jtarchie/steps/internal/mcp"
	"github.com/jtarchie/steps/internal/sealbox"
	"github.com/jtarchie/steps/internal/web"
)

func testKey(t testing.TB) string {
	t.Helper()

	key := make([]byte, 32)
	_, _ = rand.Read(key)

	return base64.StdEncoding.EncodeToString(key)
}

// installTestLogins sets a fresh key and installs the sealed vault over state, as a command would, putting back whatever was there before.
//
// Not for t.Parallel() tests: the key is process environment and the vault process state.
func installTestLogins(t *testing.T, state State, register bool) *sealedLogins {
	t.Helper()

	t.Setenv(sealbox.KeyEnv, testKey(t))
	t.Setenv(sealbox.OldKeyEnv, "")

	previous := stepsmcp.UseVault(nil)

	logins, err := useLogins(state, register)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { stepsmcp.UseVault(previous) })

	return logins
}

func loginJSON(t testing.TB, tf *stepsmcp.TokenFile) []byte {
	t.Helper()

	data, err := json.Marshal(tf) //nolint:gosec // G117: an invented token, which is what this fixture is for
	if err != nil {
		t.Fatal(err)
	}

	return data
}

// rawCredentials is every sealed value the file holds, read past the store so nothing is decrypted on the way.
func rawCredentials(t *testing.T, path string) [][]byte {
	t.Helper()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = db.Close() }()

	rows, err := db.QueryContext(t.Context(), `SELECT sealed FROM credentials`)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = rows.Close() }()

	var all [][]byte

	for rows.Next() {
		var sealed []byte

		err = rows.Scan(&sealed)
		if err != nil {
			t.Fatal(err)
		}

		all = append(all, sealed)
	}

	return all
}

// The point of the feature: a copy of the database holds no token anybody can read.
func TestALoginIsSealedInTheStateDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steps.db")
	logins := installTestLogins(t, State(path), true)

	err := logins.Save(t.Context(), "app", "tracker", "", loginJSON(t, &stepsmcp.TokenFile{AccessToken: secretToken, RefreshToken: "refresh-" + secretToken}))
	if err != nil {
		t.Fatal(err)
	}

	stored := rawCredentials(t, path)
	if len(stored) != 1 {
		t.Fatalf("the file holds %d credentials, want 1", len(stored))
	}

	if bytes.Contains(stored[0], []byte(secretToken)) {
		t.Fatal("the state database holds the token in the clear")
	}

	plain, _, found, err := logins.Load(t.Context(), "app", "tracker")
	if err != nil || !found || !bytes.Contains(plain, []byte(secretToken)) {
		t.Fatalf("Load = %q, %v, %v; want the login back", plain, found, err)
	}
}

func TestNoKeyRefusesALoginAndWritesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steps.db")
	installTestLogins(t, State(path), true)
	t.Setenv(sealbox.KeyEnv, "")

	logins, err := useLogins(State(path), true)
	if err != nil {
		t.Fatal(err)
	}

	err = logins.Save(t.Context(), "app", "tracker", "", []byte(`{}`))
	if !errors.Is(err, sealbox.ErrNoKey) || !strings.Contains(err.Error(), sealbox.KeyEnv) {
		t.Fatalf("Save with no key = %v, want ErrNoKey naming %s", err, sealbox.KeyEnv)
	}

	_, err = os.Stat(path)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a refused save still created the state database: %v", err)
	}

	if loginsReady() == nil {
		t.Error("a daemon with no key reports it can save logins")
	}
}

// A refresh landing after a destroy must not resurrect the pipeline, or the next set under its name inherits a live token.
func TestADaemonSavesOnlyForPipelinesItHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steps.db")
	logins := installTestLogins(t, State(path), false)

	err := logins.Save(t.Context(), "gone", "tracker", "", []byte(`{}`))
	if err == nil {
		t.Fatal("a daemon saved a login for a pipeline it does not hold")
	}

	_, err = openExistingState(State(path), "gone")
	if !noSuchState(err) {
		t.Errorf("the refused save left pipeline %q behind: %v", "gone", err)
	}
}

func TestAKeyRotationReSealsOnRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steps.db")
	installTestLogins(t, State(path), true)
	old := testKey(t)
	t.Setenv(sealbox.KeyEnv, old)

	logins, err := useLogins(State(path), true)
	if err != nil {
		t.Fatal(err)
	}

	err = logins.Save(t.Context(), "app", "tracker", "", []byte(`{"access_token":"a"}`))
	if err != nil {
		t.Fatal(err)
	}

	rotated := testKey(t)
	t.Setenv(sealbox.KeyEnv, rotated)

	newOnly, err := useLogins(State(path), true)
	if err != nil {
		t.Fatal(err)
	}

	_, _, _, err = newOnly.Load(t.Context(), "app", "tracker")
	if !errors.Is(err, sealbox.ErrWrongKey) {
		t.Fatalf("Load under a new key alone = %v, want ErrWrongKey", err)
	}

	t.Setenv(sealbox.OldKeyEnv, old)

	rotating, err := useLogins(State(path), true)
	if err != nil {
		t.Fatal(err)
	}

	_, _, found, err := rotating.Load(t.Context(), "app", "tracker")
	if err != nil || !found {
		t.Fatalf("Load during a rotation = found %v, %v", found, err)
	}

	t.Setenv(sealbox.OldKeyEnv, "")

	_, _, found, err = newOnly.Load(context.Background(), "app", "tracker")
	if err != nil || !found {
		t.Errorf("Load under the new key after a rotating read = found %v, %v; want it re-sealed", found, err)
	}
}

// No poll heals a login no key here opens, so preflight must neither wait on it as transient nor cache it past the login that fixes it.
func TestAnUnopenableLoginNeedsALogin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steps.db")

	err := installTestLogins(t, State(path), true).Save(t.Context(), "app", "tracker", "", []byte(`{"access_token":"a"}`))
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv(sealbox.KeyEnv, testKey(t))

	other, err := useLogins(State(path), true)
	if err != nil {
		t.Fatal(err)
	}

	_, _, _, err = other.Load(t.Context(), "app", "tracker")
	if !errors.Is(err, sealbox.ErrWrongKey) || !errors.Is(err, stepsmcp.ErrNeedsLogin) {
		t.Errorf("Load under another key = %v, want ErrWrongKey classed as ErrNeedsLogin", err)
	}
}

// A sealed row moved onto another pipeline's is a credential that pipeline was never given.
func TestARowCopiedOntoAnotherPipelineDoesNotOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steps.db")
	logins := installTestLogins(t, State(path), true)

	for _, name := range []string{"app", "other"} {
		err := logins.Save(t.Context(), name, "tracker", "", []byte(`{"access_token":"`+name+`"}`))
		if err != nil {
			t.Fatal(err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.ExecContext(t.Context(), `UPDATE credentials SET sealed = (SELECT sealed FROM credentials c JOIN pipelines p ON p.id = c.pipeline_id WHERE p.name = 'app') WHERE pipeline_id = (SELECT id FROM pipelines WHERE name = 'other')`)
	_ = db.Close()

	if err != nil {
		t.Fatal(err)
	}

	_, _, _, err = logins.Load(t.Context(), "other", "tracker")
	if !errors.Is(err, sealbox.ErrWrongKey) {
		t.Errorf("Load of app's row copied onto other = %v, want ErrWrongKey", err)
	}
}

// Refused before a browser is sent anywhere: granted and then refused, the consent is wasted.
func TestADaemonWithNoKeyRefusesToStartALogin(t *testing.T) {
	t.Setenv(sealbox.KeyEnv, "")
	t.Setenv(sealbox.OldKeyEnv, "")

	held := servingDaemon(t)
	held.ready = loginsReady
	setPipeline(t, held, "app", mcpStatePipeline)

	_, err := held.StartLogin(held.server.Lookup("app"), "tracker", web.LoginRequest{Base: "https://steps.example.com"})
	if !errors.Is(err, web.ErrRefused) || !strings.Contains(err.Error(), sealbox.KeyEnv) {
		t.Fatalf("StartLogin with no key = %v, want refused naming %s", err, sealbox.KeyEnv)
	}

	if _, found := held.LoginStatus(held.server.Lookup("app"), "tracker"); found {
		t.Error("a refused login was left pending")
	}
}

// readThenDestroy saves app's login, reads it back as a refresh would, and destroys app, handing back the identity it was read under.
func readThenDestroy(t *testing.T, path string) (*sealedLogins, string) {
	t.Helper()

	logins := installTestLogins(t, State(path), false)

	st, err := openState(State(path), "app")
	if err != nil {
		t.Fatal(err)
	}

	err = logins.Save(t.Context(), "app", "tracker", "", []byte(`{"access_token":"a"}`))
	if err != nil {
		t.Fatal(err)
	}

	_, scope, found, err := logins.Load(t.Context(), "app", "tracker")
	if err != nil || !found || scope == "" {
		t.Fatalf("Load = scope %q, found %v, %v", scope, found, err)
	}

	err = st.Delete(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	release(st)

	return logins, scope
}

// A refresh landing after a destroy must not bring the pipeline back: the next set under its name would inherit the token.
func TestAWriteBackAfterADestroyResurrectsNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steps.db")
	logins, scope := readThenDestroy(t, path)

	err := logins.Save(t.Context(), "app", "tracker", scope, []byte(`{"access_token":"rotated"}`))
	if err == nil {
		t.Fatal("a write-back for a destroyed pipeline was saved")
	}

	_, err = openExistingState(State(path), "app")
	if !noSuchState(err) {
		t.Errorf("the write-back brought app back: %v", err)
	}
}

// Nor may it land on a stranger that took the name.
func TestAWriteBackDoesNotLandOnAPipelineThatTookTheName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steps.db")
	logins, scope := readThenDestroy(t, path)

	again, err := openState(State(path), "app")
	if err != nil {
		t.Fatal(err)
	}

	release(again)

	err = logins.Save(t.Context(), "app", "tracker", scope, []byte(`{"access_token":"rotated"}`))
	if !errors.Is(err, stepsmcp.ErrLoginOrphaned) {
		t.Fatalf("a write-back onto the new app = %v, want ErrLoginOrphaned", err)
	}

	_, _, found, err := logins.Load(t.Context(), "app", "tracker")
	if err != nil || found {
		t.Errorf("the new app holds the old one's login: found %v, %v", found, err)
	}
}
