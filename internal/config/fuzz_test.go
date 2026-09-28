package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const canary = "CANARY-daemon-local-file-7f3a"

var docsFence = regexp.MustCompile("(?s)```yaml[^\n]*\n(.*?)```")

func addPipelineSeeds(f *testing.F) {
	f.Helper()

	var sources []string

	for _, pattern := range []string{"testdata/*.yml", "../../examples/*.yml"} {
		files, _ := filepath.Glob(pattern)
		for _, file := range files {
			data, err := os.ReadFile(file) //nolint:gosec // repo-owned seed files
			if err == nil {
				sources = append(sources, string(data))
			}
		}
	}

	docs, _ := filepath.Glob("../../docs/*.md")
	for _, file := range docs {
		data, err := os.ReadFile(file) //nolint:gosec // repo-owned seed files
		if err != nil {
			continue
		}

		for _, match := range docsFence.FindAllSubmatch(data, -1) {
			sources = append(sources, string(match[1]))
		}
	}

	for _, source := range sources {
		f.Add(source, "echo hello\n")
	}

	f.Add("jobs:\n- name: j\n  plan:\n  - task: t\n    run_file: canary.sh\n", "")
	f.Add("jobs:\n- name: j\n  plan:\n  - task: t\n    run_file: ../canary.sh\n", "")
	f.Add("jobs:\n- name: j\n  plan:\n  - task: t\n    run_file: tasks/hello.sh\n", "echo hi\n")
	f.Add("a: &a [*a, *a]\n", "")
}

// FuzzParse is the daemon's `pipeline set` door: arbitrary YAML plus a bundle of includes, parsed on a machine whose own disk must never leak in.
func FuzzParse(f *testing.F) {
	addPipelineSeeds(f)

	dir := f.TempDir()
	canaryPath := filepath.Join(dir, "canary.sh")

	for _, name := range []string{canaryPath, filepath.Join(filepath.Dir(dir), "canary.sh")} {
		_ = os.WriteFile(name, []byte(canary), 0o600)
	}

	f.Add("jobs:\n- name: j\n  plan:\n  - task: t\n    run_file: "+canaryPath+"\n", "")
	f.Chdir(dir)

	previous := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	f.Cleanup(func() { slog.SetDefault(previous) })

	f.Fuzz(func(t *testing.T, source, include string) {
		if len(source) > 64<<10 || strings.Contains(source+include, canary) {
			t.Skip()
		}

		bundle := Bundle{"tasks/hello.sh": include, "ci/app.yml": include}

		first, firstErr := Parse([]byte(source), "fuzz", bundle)
		second, secondErr := Parse([]byte(source), "fuzz", bundle)

		if fmt.Sprint(firstErr) != fmt.Sprint(secondErr) {
			t.Fatalf("parse is not deterministic: %v vs %v", firstErr, secondErr)
		}

		if firstErr != nil {
			if strings.Contains(firstErr.Error(), canary) {
				t.Fatalf("an error quotes a daemon-local file: %v", firstErr)
			}

			return
		}

		checkParsed(t, source, bundle, first, second)
	})
}

func checkParsed(t *testing.T, source string, bundle Bundle, first, second *Config) {
	t.Helper()

	if first.Revision.SHA == "" || first.Revision.SHA != second.Revision.SHA {
		t.Fatalf("revision is not deterministic: %q vs %q", first.Revision.SHA, second.Revision.SHA)
	}

	if first.Revision.Source != source {
		t.Fatal("the revision does not record the source it was parsed from")
	}

	for name, content := range first.Revision.Includes {
		sent, err := bundle.ReadFile(name)
		if err != nil || string(sent) != content {
			t.Fatalf("include %q was resolved from somewhere other than the bundle", name)
		}
	}

	encoded, err := yaml.Marshal(first) //nolint:musttag // searched for the canary only; an untagged field marshals under its Go name, which is enough
	if err == nil && bytes.Contains(encoded, []byte(canary)) {
		t.Fatal("a daemon-local file reached the parsed config")
	}
}

func FuzzValidPipelineName(f *testing.F) {
	for _, seed := range []string{"build", "..", "a/b", "a?b", "%2e", "ci.main_1-x", ""} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, name string) {
		if ValidPipelineName(name) != nil {
			return
		}

		if url.PathEscape(name) != name || url.QueryEscape(name) != name {
			t.Fatalf("accepted %q, which changes when escaped into a URL", name)
		}

		if name == "." || name == ".." || !filepath.IsLocal(name) || strings.ContainsAny(name, `/\`) || len(name) > maxPipelineNameLength {
			t.Fatalf("accepted %q, which is not one safe path segment", name)
		}
	})
}

func FuzzValidateArtifactPath(f *testing.F) {
	for _, seed := range []string{"repo", "findings/alpha/fast", "../x", "a//b", "a/./b", "a/", UpstreamDir, ".hidden", "a/..", `a\b`} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, artifactPath string) {
		nameErr := ValidateArtifactName(artifactPath)
		pathErr := ValidateArtifactPath(artifactPath)

		if nameErr == nil && pathErr != nil {
			t.Fatalf("%q is a valid name but not a valid path: %v", artifactPath, pathErr)
		}

		if pathErr != nil {
			return
		}

		checkJoinable(t, artifactPath, nameErr == nil)
	})
}

func checkJoinable(t *testing.T, artifactPath string, isName bool) {
	t.Helper()

	if !filepath.IsLocal(artifactPath) || path.Clean(artifactPath) != artifactPath || strings.Contains(artifactPath, `\`) {
		t.Fatalf("accepted %q, which does not stay under the directory it is joined to", artifactPath)
	}

	first, _, _ := strings.Cut(artifactPath, "/")
	if first == UpstreamDir {
		t.Fatalf("accepted %q under the reserved %q", artifactPath, UpstreamDir)
	}

	if isName && strings.Contains(artifactPath, "/") {
		t.Fatalf("accepted %q as a single artifact name", artifactPath)
	}
}

func FuzzRenderVars(f *testing.F) {
	f.Add("uri: ((repo_uri))/x", "repo_uri", "https://example.com")
	f.Add("((((k))))", "k", "k")
	f.Add("((a))((b))", "a", "((b))")
	f.Add("no vars", "x", "y")

	f.Fuzz(func(t *testing.T, value, key, replacement string) {
		if RenderVars(value, nil) != value || string(InterpolateVars([]byte(value), map[string]string{})) != value {
			t.Fatalf("rendering %q with no vars changed it", value)
		}

		rendered := RenderVars(value, map[string]string{key: replacement})
		if string(InterpolateVars([]byte(value), map[string]string{key: replacement})) != rendered {
			t.Fatalf("RenderVars and InterpolateVars disagree on %q", value)
		}

		names := UnresolvedVars(value)

		all := map[string]string{}
		for _, name := range names {
			if !varPattern.MatchString("((" + name + "))") {
				t.Fatalf("UnresolvedVars(%q) returned %q, which is not a var reference", value, name)
			}

			all[name] = "@"
		}

		if left := UnresolvedVars(RenderVars(value, all)); len(left) > 0 {
			t.Fatalf("rendering every var in %q left %v behind", value, left)
		}
	})
}

func FuzzParseTimeout(f *testing.F) {
	for _, seed := range []string{"", "0", "2m", "1h30m", "-1s", "2 minutes", "9223372036854775807ns", "1.5h"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		d, err := ParseTimeout(s)
		if err != nil {
			return
		}

		if d < 0 {
			t.Fatalf("ParseTimeout(%q) = %v, negative", s, d)
		}

		again, err := ParseTimeout(d.String())
		if err != nil || again != d {
			t.Fatalf("%q parsed to %v, whose String does not parse back: %v, %v", s, d, again, err)
		}

		if ResolvedAgentTimeout(s) != d && s != "" {
			t.Fatalf("ResolvedAgentTimeout(%q) disagrees with ParseTimeout", s)
		}
	})
}

func FuzzSlugify(f *testing.F) {
	for _, seed := range []string{"pipeline.yml", "ci/deploy.prod.yml", ".yml", "/", "", "a/"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, p string) {
		got := Slugify(p)

		if !strings.HasPrefix(filepath.Base(p), got) || Slugify(filepath.Base(p)) != got {
			t.Fatalf("Slugify(%q) = %q, not derived from the file name alone", p, got)
		}
	})
}

// FuzzIsPlainBranchName holds the Go rules to git's own: a name steps accepts must be one git accepts, since it is spliced into a refspec.
func FuzzIsPlainBranchName(f *testing.F) {
	git, err := exec.LookPath("git")
	if err != nil {
		f.Skip("git is not on PATH")
	}

	for _, seed := range []string{"main", "feature/x", "a..b", "-rf", "x.lock", "a/.b", "@", "a@{1}", "a\x7f"} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, branch string) {
		// validateGitFetch refuses an empty branch before asking this.
		if branch == "" || !isPlainBranchName(branch) {
			return
		}

		out, err := exec.CommandContext(t.Context(), git, "check-ref-format", "--branch", branch).CombinedOutput() //nolint:gosec // the fuzz input is the argument under test
		if err != nil {
			t.Fatalf("accepted %q, which git refuses: %s", branch, out)
		}
	})
}

// FuzzValidateGitFetch holds source.fetch to refreshing a LOCAL clone: whatever uri and branch it accepts, the uri is an absolute path and the branch cannot become an option or a second refspec.
func FuzzValidateGitFetch(f *testing.F) {
	for _, seed := range [][2]string{
		{"/repo", "main"},
		{"git@github.com:org/repo.git", "main"},
		{"https://github.com/org/repo", "main"},
		{"/repo", "-upload-pack=x"},
		{"/repo", "refs/heads/main"},
		{"repo:/x", "main"},
	} {
		f.Add(seed[0], seed[1])
	}

	f.Fuzz(func(t *testing.T, uri, branch string) {
		if validateGitFetch("r", uri, branch) != nil {
			return
		}

		if !filepath.IsAbs(uri) || strings.Contains(uri, "://") {
			t.Fatalf("accepted %q as a local clone", uri)
		}

		if branch == "" || strings.HasPrefix(branch, "-") || strings.HasPrefix(branch, "refs/") || strings.ContainsAny(branch, ": ") {
			t.Fatalf("accepted branch %q, which is not a plain branch name", branch)
		}
	})
}

func FuzzDecodeSource(f *testing.F) {
	f.Add("expression: '*/5 * * * *'\nlocation: America/New_York\n")
	f.Add("expr: body.ref == 'main'\n")
	f.Add("expression: 1\nunknown: 2\n")
	f.Add("location: ../../etc/passwd\n")

	f.Fuzz(func(t *testing.T, raw string) {
		var source map[string]any
		if yaml.Unmarshal([]byte(raw), &source) != nil || len(raw) > 4<<10 {
			return
		}

		cron, ok := decodeTwice[CronSource](t, source)
		if ok {
			location, err := cron.TimeLocation()
			if err == nil && (location == nil || strings.Contains(cron.Location, "..") || filepath.IsAbs(cron.Location)) {
				t.Fatalf("location %q resolved outside the zoneinfo database", cron.Location)
			}
		}

		decodeTwice[WebhookSource](t, source)
	})
}

func decodeTwice[T any](t *testing.T, source map[string]any) (T, bool) {
	t.Helper()

	var first, second T
	if decodeSource(source, &first) != nil {
		return first, false
	}

	if decodeSource(source, &second) != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("decoding %v is not deterministic", source)
	}

	return first, true
}
