package cli

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/store"
	"github.com/jtarchie/steps/internal/store/postgres"
	"github.com/jtarchie/steps/internal/trigger"
)

// StateFlags are the two flags every command that opens a state database
// carries: WHERE the database is, and WHAT this pipeline is called inside it.
//
// Embedded rather than repeated because they always travel together — a
// --db naming a shared file is exactly when --name starts to matter.
type StateFlags struct {
	DB   DB                `help:"state database: a sqlite file path, sqlite:// or postgres:// url (default: .steps/<pipeline>.db beside the YAML)" name:"db"   placeholder:"URL"`
	Name map[string]string `help:"name a pipeline inside the state db, e.g. --name infra=infra/pipeline.yml (repeatable)"                           name:"name"`
}

// ReadFlags is what a command that only READS a daemon's database needs: which database, and which pipeline in it.
//
// A NAME rather than a path, because a served pipeline no longer has a file
// here — `steps pipeline set` uploaded it, and the name is what the daemon,
// the /p/<slug> route and every row it wrote agree on. The database defaults
// to the daemon's own for the same reason: there is no YAML to derive one from.
type ReadFlags struct {
	DB       DB     `help:"state database: a sqlite file path, sqlite:// or postgres:// url (default: .steps/steps.db)" name:"db"       placeholder:"URL"`
	Pipeline string `help:"the pipeline's name in the state database"                                                   name:"pipeline" short:"p"`
}

// state is the database these flags name.
func (r ReadFlags) state() State { return DaemonStatePath(r.DB) }

// DB is the --db value: the state database as a bare sqlite path or as a url
// whose scheme picks the driver, the way --worker's ssh:// and aws:// pick a
// transport — sqlite:// or postgres:// (postgresql:// too). The switch is here
// in the CLI rather than in internal/store because the contract package
// cannot import its own drivers; db.go is where it is made.
//
// Checked while the flag is parsed, so an unknown scheme is a usage error
// before any command runs: opened as a file, `mysql://…` would have been a
// freshly created sqlite database of that name, and a job recorded into it.
//
// No refusal here repeats the value: a network url carries credentials, and a
// usage error lands in shell history and CI logs.
type DB string

// UnmarshalText is kong's parse hook for the flag.
func (d *DB) UnmarshalText(text []byte) error {
	raw := string(text)

	scheme, _, _ := strings.Cut(raw, ":")

	var err error
	if postgresScheme(scheme) {
		err = checkPostgresURL(raw)
	} else {
		err = checkSQLiteDB(raw)
	}

	if err != nil {
		return err
	}

	*d = DB(raw)

	return nil
}

// checkPostgresURL refuses a url that will not parse — by url.Parse's
// standard rather than pgx's, and without url.Parse's own error, which quotes
// the url — and warns about a password in it.
func checkPostgresURL(raw string) error {
	if !strings.Contains(raw, "://") {
		// Otherwise a sqlite FILE of that name, created beside the YAML.
		return errors.New("postgres: needs //: write postgres://… (a unix socket is postgres:///<db>?host=/var/run/postgresql)")
	}

	_, err := url.Parse(raw)
	if err != nil {
		return errors.New("--db: the postgres url does not parse; check its syntax (not repeated here, as it may carry a password)")
	}

	warnPasswordInURL(raw)

	return nil
}

// checkSQLiteDB refuses what is neither a sqlite file nor a url a driver
// opens.
func checkSQLiteDB(raw string) error {
	scheme, rest, isURL := strings.Cut(raw, "://")

	switch {
	case isURL && scheme != "sqlite":
		return fmt.Errorf("no driver for %s:// (a sqlite file path, sqlite://<path>, or postgres://…)", scheme)
	case isURL && rest == "":
		// Stripped to "", the bare scheme would read as the flag not given
		// and quietly open the per-pipeline default instead.
		return errors.New("sqlite:// names no file: write sqlite://<path>")
	case !isURL && strings.HasPrefix(raw, "sqlite:"):
		return errors.New("sqlite: needs //: write sqlite://<path>")
	case strings.Contains(raw, "?"):
		// The sqlite driver splits its DSN at the first '?', so a query
		// string here swallows the pragmas steps sets after the file name
		// (busy_timeout first) with no error — and the read commands stat a
		// file of that whole name and report nothing recorded.
		return errors.New("--db takes a sqlite file path or sqlite://<path> with no query string, or a postgres:// url")
	}

	return nil
}

// location is the database a DB names — a sqlite file, or a postgres url
// as given — or "" for the default.
func (d *DB) location() State {
	if State(*d).postgres() {
		return State(*d)
	}

	path, _ := strings.CutPrefix(string(*d), "sqlite://")

	return State(path)
}

func postgresScheme(scheme string) bool { return scheme == "postgres" || scheme == "postgresql" }

// State is a resolved state database: a sqlite file path, or a postgres url.
//
// A type rather than a string so that printing one is safe by default: a url
// may carry a password, and String — what %s and %v call — never shows it.
// Opening one goes through db.go, which is the one place that reads the raw
// value.
type State string

// String is the state database as it may be printed.
func (s State) String() string {
	if s.postgres() {
		return postgres.Redact(string(s))
	}

	return string(s)
}

func (s State) postgres() bool {
	scheme, _, isURL := strings.Cut(string(s), "://")

	return isURL && postgresScheme(scheme)
}

// VarFlags carry ((name)) substitutions into a pipeline load.
//
// Declared once and embedded rather than repeated, because seven commands
// take them and a copy that drifts is a flag that silently means something
// else on one verb. Load is part of the embed for the same reason: a command
// that takes these flags and loads the pipeline some other way has declared
// an input it does not read, which is the shape of bug this consolidation
// exists to make impossible.
type VarFlags struct {
	Var      map[string]string `help:"set a pipeline var, e.g. --var repo_uri=https://..." name:"var"       short:"v"`
	VarsFile string            `help:"YAML file of pipeline vars"                          name:"vars-file"`
}

// Load reads the pipeline at path, under the identity name, with these vars
// applied.
//
// name is passed rather than derived from path because --name lives on
// StateFlags and this embed cannot see it: a caller resolves the identity once
// (resolvePipelineName) and hands the same string to the store and to here, so
// the two cannot disagree. Positional for the reason config.Load makes it so.
func (v VarFlags) Load(path string, name string) (*config.Config, error) {
	return loadWithVars(path, name, v.Var, v.VarsFile)
}

// ExecFlags shape how a command that RUNS steps executes them: which machines
// tags: resolve to, where cached outputs are mirrored, what a parked question
// is answered with, whether the workspace survives, and whether the pre-run
// health check happens at all.
//
// One embed rather than five, because they are one idea — what the operator
// asked for about this execution — and because Apply is then the single place
// that threads them, which no embedder can forget without failing to compile.
type ExecFlags struct {
	Worker        map[string]string `help:"map a step tag to a worker, e.g. --worker gpu=ssh://jt@box (repeatable)"                                   name:"worker"`
	ArtifactStore string            `help:"mirror cached step outputs to a content-addressed store, e.g. --artifact-store s3://bucket/prefix"         name:"artifact-store"`
	Answer        []string          `help:"answer an ask_user question in advance, e.g. --answer 'which bump=minor' (repeatable)"                     name:"answer"`
	KeepWorkspace bool              `env:"STEPS_KEEP_WORKSPACE"                                                                                       help:"leave the build workspace on disk instead of deleting it"`
	NoPreflight   bool              `help:"skip the pre-run health check of the models and MCP servers a job needs"                                   name:"no-preflight"`
	Deliver       map[string]string `help:"record a captured request as a delivery to a webhook resource, e.g. --deliver push=push.http (repeatable)" name:"deliver"`
}

// prepare is Apply plus what needs the loaded pipeline and its store: the --deliver files, recorded before anything runs so the job resolves each as its resource's newest version.
func (e ExecFlags) prepare(ctx context.Context, cfg *config.Config, st store.Store) (context.Context, error) {
	ctx, err := e.Apply(ctx)
	if err != nil {
		return ctx, err
	}

	return ctx, e.deliver(ctx, cfg, st)
}

// deliver records each --deliver file.
func (e ExecFlags) deliver(ctx context.Context, cfg *config.Config, st store.Store) error {
	for _, name := range slices.Sorted(maps.Keys(e.Deliver)) {
		raw, err := os.ReadFile(e.Deliver[name])
		if err != nil {
			return fmt.Errorf("--deliver %s: %w", name, err)
		}

		err = trigger.DeliverLocally(ctx, cfg, st, name, raw)
		if err != nil {
			return err //nolint:wrapcheck // DeliverLocally names the flag and the resource
		}
	}

	return nil
}

// Apply folds these flags into the context every step below reads them from.
//
// Workers are PARSED here rather than at first use, so a typo in a worker URL
// is reported with everything else wrong with the invocation instead of
// mid-plan, when some step happens to reach for it.
func (e ExecFlags) Apply(ctx context.Context) (context.Context, error) {
	if e.NoPreflight {
		ctx = pipeline.WithoutPreflight(ctx)
	}

	ctx, err := pipeline.WithWorkers(ctx, e.Worker)
	if err != nil {
		return nil, fmt.Errorf("%w", err)
	}

	ctx = pipeline.WithArtifactStore(ctx, e.ArtifactStore)
	ctx = pipeline.WithKeepWorkspace(ctx, e.KeepWorkspace)

	ctx, err = pipeline.WithAnswers(ctx, e.Answer)
	if err != nil {
		return nil, fmt.Errorf("could not read --answer: %w", err)
	}

	return ctx, nil
}

// HistoryFlags bound what a build leaves behind.
//
// Both limits belong on every command that records: a manual run writes the
// same nodes, events and transcripts a triggered one does. --version-history
// was once declared on `run` and threaded nowhere, which is why declaring and
// applying are the same embed now.
type HistoryFlags struct {
	VersionHistory int `help:"how many versions of each resource to remember (pipeline defaults.version_history wins)" name:"version-history"`
	RunHistory     int `help:"how many runs of each job to keep (pipeline defaults.run_history wins)"                  name:"run-history"`
}

// Apply lets the flags stand in for limits the pipeline did not set. The
// pipeline wins where it spoke: it is the thing under version control.
func (h HistoryFlags) Apply(cfg *config.Config) {
	applyHistoryFlags(cfg, h.VersionHistory, h.RunHistory)
}

// DefaultDaemonState is where a daemon and the commands that read its
// database look when --db says nothing.
//
// A fixed name rather than one derived from a file, because there is no file:
// a daemon is handed pipelines by `steps pipeline set`, and the commands that
// read what it recorded are handed a NAME. See docs/web.md.
const DefaultDaemonState = ".steps/steps.db"

// DaemonStatePath is the state database `steps web` and the read commands use.
func DaemonStatePath(db DB) State {
	if location := db.location(); location != "" {
		return location
	}

	return DefaultDaemonState
}

// StatePath returns the sqlite database path for pipeline's persisted job
// state: under .steps/ beside the pipeline YAML, named for the FILE — unless
// --db names one, which is how several pipelines come to share a file.
// A sqlite:// url and the bare path it wraps are one file.
//
// Per file BY DEFAULT, not per directory, and that default is load-bearing on
// its own. Two pipelines in one folder are two namespaces, and a `.steps/state.db`
// that merged them by accident of layout was a bug: one pipeline's version
// change could enqueue a job the other then claimed and ran. Sharing is now
// something an operator asks for, and the database keeps them apart when they
// do — every row is scoped to a pipelines row (see internal/store/schema.go).
//
// There is no migration, per this repo's no-migration rule: a database from an
// older schema is refused rather than upgraded.
func StatePath(pipeline string, db DB) State {
	if location := db.location(); location != "" {
		return location
	}

	return State(filepath.Join(filepath.Dir(pipeline), ".steps", filepath.Base(pipeline)+".db"))
}

// answerDB is the --db a parked step's printed command needs for a local run's state, and "" when that is the daemon default the read commands open anyway. Quoted for a shell, since the command is printed to be pasted.
func answerDB(pipelinePath string, db DB) string {
	state := StatePath(pipelinePath, db)
	if state.isDefaultDaemonState() {
		return ""
	}

	return shellArg(state.String()) // ponytail: as resolved, so a relative path reaches it only from where the run started; filepath.Abs it if answers come from elsewhere
}

// isDefaultDaemonState reports the database the read commands open with no
// --db. A url is never it, and never goes near filepath, which would fold
// postgres:// into postgres:/.
func (s State) isDefaultDaemonState() bool {
	return !s.postgres() && filepath.Clean(string(s)) == DefaultDaemonState
}

// PipelineName is a pipeline's identity inside a state database: the YAML's
// base name without its extension.
//
// The same string web.Slugify and config.Slugify produce, and deliberately so
// — the UI's /p/<slug> route, the database's pipelines.name and the Config's
// own name are one identity, not three that have to be kept in agreement.
func PipelineName(pipeline string) string {
	return config.Slugify(pipeline)
}

// resolvePipelineName applies the --name overrides to one pipeline path.
//
// The map is keyed by NAME, matching how it is typed (--name infra=infra/ci.yml)
// and giving uniqueness for free: two paths cannot claim one name, because the
// second assignment would replace the first rather than collide silently.
// Nothing matching means the default — the base name — which is what makes the
// flag needed only when a shared --db has two pipeline.yml in it.
func resolvePipelineName(pipeline string, names map[string]string) string {
	want, err := filepath.Abs(pipeline)
	if err != nil {
		want = filepath.Clean(pipeline)
	}

	for name, path := range names {
		got, err := filepath.Abs(path)
		if err != nil {
			got = filepath.Clean(path)
		}

		if got == want {
			return name
		}
	}

	return PipelineName(pipeline)
}

// applyHistoryFlags writes the command-line retention limits into the config
// unless the pipeline set its own.
//
// Precedence is resolved here rather than at the point of use so there is one
// place it lives: the pipeline wins, because it is the thing that knows what its
// resources and its jobs do. See config.VersionHistoryLimit and
// config.RunHistoryLimit.
func applyHistoryFlags(cfg *config.Config, versions, runs int) {
	if versions <= 0 && runs <= 0 {
		return
	}

	if cfg.Defaults == nil {
		cfg.Defaults = &config.Defaults{}
	}

	if versions > 0 && cfg.Defaults.VersionHistory == nil {
		cfg.Defaults.VersionHistory = &versions
	}

	if runs > 0 && cfg.Defaults.RunHistory == nil {
		cfg.Defaults.RunHistory = &runs
	}
}

// loadWithVars loads a pipeline with ((name)) substitution applied, from
// --var flags and an optional --vars-file.
//
// Flags win over the file: the file is the shared, checked-in set and the flag
// is the one-off override, which is the only ordering that makes overriding
// possible at all.
func loadWithVars(path string, name string, flags map[string]string, varsFile string) (*config.Config, error) {
	vars, err := resolveVars(flags, varsFile)
	if err != nil {
		return nil, err
	}

	cfg, err := config.Load(path, name, vars)
	if err != nil {
		return nil, fmt.Errorf("could not load pipeline: %w", err)
	}

	return cfg, nil
}

// resolveVars gathers the ((name)) substitutions from --vars-file and --var.
//
// Flags win over the file: the file is the shared, checked-in set and the flag
// is the one-off override, which is the only ordering that makes overriding
// possible at all.
func resolveVars(flags map[string]string, varsFile string) (map[string]string, error) {
	vars := map[string]string{}

	if varsFile != "" {
		body, err := os.ReadFile(varsFile) //nolint:gosec // the vars file is one the operator named
		if err != nil {
			return nil, fmt.Errorf("could not read vars file %q: %w", varsFile, err)
		}

		var fromFile map[string]string

		err = yaml.Unmarshal(body, &fromFile)
		if err != nil {
			return nil, fmt.Errorf("could not parse vars file %q: %w", varsFile, err)
		}

		for name, value := range fromFile {
			vars[name] = value
		}
	}

	for name, value := range flags {
		vars[name] = value
	}

	return vars, nil
}

// openStore opens a pipeline's already-recorded state for the approval,
// question and paused-job commands.
//
// The same resolve-never-register path `steps runs` takes, and for the same
// reason one level along: every caller here acts on a row that must already
// exist — an approval to decide, a question to answer, a breaker to read — so
// none of them has any business CREATING the pipeline it was named. It used
// to go through setup, which opened the store the writer's way and minted a
// pipelines row for a typo; worse, it did so in a state file `steps runs` had
// just learned to refuse that name in, so one listing quietly made the
// refusal stop working.
//
// It also builds no workspace provider, which setup did — a listing that
// creates and removes a temp root to print three rows.
func openStore(flags ReadFlags) (store.Store, func(), error) {
	return openRecorded(flags)
}
