package postgres

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// connConfig turns a --db URL into a connection config pinned to this
// driver's schema.
//
// search_path is set to that schema and NOTHING else, so an unqualified name
// can only ever resolve to steps' own table — never to a same-named table in
// public, and never to one somebody planted earlier in the path. A URL's
// search_path chooses the schema (its first element), it does not widen it.
//
// The error never carries the URL: pgconn's own echoes it redacted
// best-effort, and a URL that failed to parse is exactly the one whose
// password a redactor may not find.
func connConfig(rawURL string) (*pgx.ConnConfig, string, error) {
	config, err := pgx.ParseConfig(rawURL)
	if err != nil {
		return nil, "", errors.New("could not parse the postgres URL given to --db; check its syntax (the URL is not repeated here because it may carry a password)")
	}

	schema := DefaultSchema

	if path := config.RuntimeParams["search_path"]; path != "" {
		first, _, _ := strings.Cut(path, ",")
		first = strings.TrimSpace(first)

		if unquoted, ok := strings.CutPrefix(first, `"`); ok {
			first = strings.ReplaceAll(strings.TrimSuffix(unquoted, `"`), `""`, `"`)
		}

		if first != "" {
			schema = first
		}
	}

	// A url copied from another application's config often says
	// search_path=public, or libpq's default "$user", public — and either
	// would put steps' tables beside that application's.
	if schema == "public" || schema == "$user" {
		return nil, "", fmt.Errorf("--db names schema %s: steps keeps its tables out of %s; set search_path to a schema of its own, or leave it out for %q", schema, schema, DefaultSchema)
	}

	config.RuntimeParams["search_path"] = quote(schema)
	// Timestamps come back in the session's zone; UTC is what every reader
	// here compares them against.
	config.RuntimeParams["TimeZone"] = "UTC"

	// What pg_stat_activity and the lock views call these sessions, so an
	// operator reading who holds a pipeline's advisory lock sees steps and
	// not a bare backend. A url that names one keeps it.
	if config.RuntimeParams["application_name"] == "" {
		config.RuntimeParams["application_name"] = "steps"
	}

	// pgx waits forever by default, and the event sink and Close run on a
	// context nobody cancels: a blackholed host would hang the open for good.
	if config.ConnectTimeout == 0 {
		config.ConnectTimeout = 10 * time.Second
	}

	return config, schema, nil
}

func quote(identifier string) string { return pgx.Identifier{identifier}.Sanitize() }

// describeConfig is Description: the URL rebuilt from what the config
// resolved, which carries no password by construction, plus the schema —
// two schemas in one database are two state databases.
func describeConfig(config *pgx.ConnConfig, schema string) string {
	described := url.URL{Scheme: "postgres", Path: "/" + config.Database}
	if config.User != "" {
		described.User = url.User(config.User)
	}

	query := url.Values{}

	if strings.HasPrefix(config.Host, "/") {
		query.Set("host", config.Host)
	} else {
		described.Host = net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port)))
	}

	query.Set("search_path", schema)
	described.RawQuery = query.Encode()

	return described.String()
}

// Redact is a --db URL safe to print: the userinfo password and the
// password= and sslpassword= parameters removed, everything else kept.
func Redact(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		// Unparseable means the boundaries a redactor needs are unknown.
		return "postgres://(unparseable URL)"
	}

	if parsed.User != nil {
		parsed.User = url.User(parsed.User.Username())
	}

	query := parsed.Query()
	if query.Has("password") || query.Has("sslpassword") {
		query.Del("password")
		query.Del("sslpassword")
		parsed.RawQuery = query.Encode()
	}

	return parsed.String()
}

// HasPassword reports a URL carrying its own password, which a process list
// and a shell history both keep.
func HasPassword(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}

	_, inUserinfo := parsed.User.Password()
	query := parsed.Query()

	return inUserinfo || query.Get("password") != "" || query.Get("sslpassword") != ""
}

// describe turns the failures an operator can fix into what to do about them.
// Everything else passes through as pgx reported it, which names the user and
// database and never the password.
func describe(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}

	switch pgErr.Code {
	case "3D000":
		return fmt.Errorf("%w — steps creates its schema but never the database; create it with `createdb` first", err)
	case "28P01", "28000":
		return fmt.Errorf("%w — set PGPASSWORD, or use ~/.pgpass or PGSERVICEFILE, rather than putting a password in --db", err)
	}

	return err
}
