package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/alecthomas/kong"
	"gopkg.in/yaml.v3"

	"github.com/jtarchie/steps/internal/secret"
)

// configFileKeysRefused: the password is a credential in a file meant to be committed (env-only, like the OAuth secrets go through --*-client-secret-env), and a config naming a config is a loop.
var configFileKeysRefused = []string{"basic-auth-password", "config"}

// BeforeResolve hooks the COMMAND, not the flag: kong fires a flag's hook for an env-set value on every command (applyHookToDefaultFlags walks the whole tree), so an exported STEPS_CONFIG must not make `steps validate` open the daemon's file; FlagValue because argv values reach the struct only after the resolvers ran.
func (w *WebCmd) BeforeResolve(ctx *kong.Context) error {
	node := ctx.Selected()
	if node == nil {
		return nil
	}

	var path string

	for _, flag := range node.Flags {
		if flag.Name == "config" {
			path, _ = ctx.FlagValue(flag).(string)
		}
	}

	if path == "" {
		return nil
	}

	values, err := loadConfigFile(path, append(ctx.Model.Flags, node.Flags...))
	if err != nil {
		return err
	}

	raw, ok := values["secret"]
	if ok {
		delete(values, "secret")

		w.secrets, err = parseSecrets(path, raw)
		if err != nil {
			return err
		}
	}

	ctx.AddResolver(fileResolver(values))

	return nil
}

// parseSecrets re-encodes the section so secret.Parse can decode it strictly; the map loadConfigFile holds has already lost YAML's unknown-key information.
func parseSecrets(path string, raw any) (*secret.Resolver, error) {
	body, err := yaml.Marshal(raw)
	if err != nil {
		return nil, fmt.Errorf("web: --config %s: %w", path, err)
	}

	resolver, err := secret.Parse(body)
	if err != nil {
		return nil, fmt.Errorf("web: --config %s: %w", path, err)
	}

	return resolver, nil
}

// loadConfigFile checks keys eagerly rather than in kong's Resolver.Validate, which runs after Resolve, so a refused key never reaches a flag first.
func loadConfigFile(path string, flags []*kong.Flag) (map[string]any, error) {
	top, err := readConfigFile(path)
	if err != nil {
		return nil, err
	}

	for key := range top {
		if key != "web" {
			return nil, fmt.Errorf("web: --config %s: unknown key %q (settings nest under web:)", path, key)
		}
	}

	web, _ := top["web"].(map[string]any)

	err = checkConfigKeys(path, web, flags)
	if err != nil {
		return nil, err
	}

	return web, nil
}

func readConfigFile(path string) (map[string]any, error) {
	file, err := os.Open(path) //nolint:gosec // the operator names this file; reading it is the point
	if err != nil {
		return nil, fmt.Errorf("web: --config: %w", err)
	}

	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("web: --config: %w", err)
	}

	// A privilege boundary: the file names commands the daemon runs as its own user, so it is held to what sshd asks of its config and keys.
	if info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("web: --config %s is writable by group or others (mode %04o); chmod 0600 it — this file configures what the daemon runs", path, info.Mode().Perm())
	}

	err = checkOwnedByUs(path, info)
	if err != nil {
		return nil, err
	}

	var top map[string]any

	err = yaml.NewDecoder(file).Decode(&top)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("web: --config %s: %w", path, err)
	}

	return top, nil
}

func checkConfigKeys(path string, web map[string]any, flags []*kong.Flag) error {
	known := make([]string, 0, len(flags))
	for _, flag := range flags {
		known = append(known, flag.Name)
	}

	for key := range web {
		// The one key that is not a flag: a source is nested objects, which no flag can carry.
		if key == "secret" {
			continue
		}

		if slices.Contains(configFileKeysRefused, key) {
			return fmt.Errorf("web: --config %s: %s may not be set in the file — it is a credential, pass it as STEPS_%s", path, key, strings.ToUpper(strings.ReplaceAll(key, "-", "_")))
		}

		if !slices.Contains(known, key) {
			return fmt.Errorf("web: --config %s: unknown key web.%s (every key is a `steps web` flag name)", path, key)
		}
	}

	return nil
}

// fileResolver hands values back raw so a YAML map or list reaches kong's map and slice decoders whole; kong's own order is flag, file, env, default.
type fileResolver map[string]any

func (r fileResolver) Resolve(_ *kong.Context, _ *kong.Path, flag *kong.Flag) (any, error) {
	value, ok := r[flag.Name]
	if !ok {
		return nil, nil //nolint:nilnil // kong's contract: nil means "no value here"
	}

	return value, nil
}

func (r fileResolver) Validate(*kong.Application) error { return nil }

// secretsFromConfigFile reads only the secret: section, for `steps validate --live` to resolve references as the daemon would. The file is held to the same ownership and mode rules, since it names commands this user then runs.
func secretsFromConfigFile(path string) (*secret.Resolver, error) {
	top, err := readConfigFile(path)
	if err != nil {
		return nil, err
	}

	web, _ := top["web"].(map[string]any)

	raw, ok := web["secret"]
	if !ok {
		return nil, fmt.Errorf("--config %s has no secret: section under web:, so there are no sources to resolve references with", path)
	}

	return parseSecrets(path, raw)
}
