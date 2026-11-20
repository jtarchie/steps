package cli

// `steps run --target --input GET=PATH` (#283): the terminal's half, which lists and packs each folder, and the daemon's, which unpacks them under a bound before the one-off is recorded.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jtarchie/steps/internal/compress"
	"github.com/jtarchie/steps/internal/config"
	"github.com/jtarchie/steps/internal/pipeline"
	"github.com/jtarchie/steps/internal/web"
	"github.com/jtarchie/steps/internal/wire"
)

// InputFlags are the folders a one-off carries in place of gets, as fly execute's -i, --include-ignored and --exclude.
type InputFlags struct {
	Input          map[string]string `help:"with --target, upload this folder in place of the named get, e.g. src=. (repeatable)" name:"input"           placeholder:"GET=PATH"`
	IncludeIgnored bool              `help:"with --input, also upload the files git ignores"                                      name:"include-ignored"`
	Exclude        []string          `help:"with --input, leave out the files a glob matches, by path or by name (repeatable)"    name:"exclude"         placeholder:"GLOB"`
}

func (f InputFlags) asked() bool {
	return len(f.Input) > 0 || f.IncludeIgnored || len(f.Exclude) > 0
}

// refuseWithoutTarget is the local run's answer: an upload replaces a get on a daemon, and a local run already reads this machine's folders.
func (f InputFlags) refuseWithoutTarget() error {
	if f.asked() {
		return errors.New("--input, --include-ignored and --exclude apply only with --target: they upload folders to the daemon's one-off")
	}

	return nil
}

// checkInputs refuses, before anything is sent, a get the job does not have or a path that is not a folder.
func (f InputFlags) checkInputs(job *config.Job) error {
	if !f.asked() {
		return nil
	}

	if len(f.Input) == 0 {
		return errors.New("--include-ignored and --exclude apply only with --input")
	}

	gets := job.Gets()

	for _, get := range slices.Sorted(maps.Keys(f.Input)) {
		if _, ok := gets[get]; !ok {
			return fmt.Errorf("--input %q: job %q has no get named %q (it has: %s)", get, job.Name, get, strings.Join(slices.Sorted(maps.Keys(gets)), ", "))
		}

		info, err := os.Stat(f.Input[get])
		if err != nil {
			return fmt.Errorf("--input %s: %w", get, err)
		}

		if !info.IsDir() {
			return fmt.Errorf("--input %s: %s is not a folder", get, f.Input[get])
		}
	}

	for _, glob := range f.Exclude {
		_, err := path.Match(glob, "")
		if err != nil {
			return fmt.Errorf("--exclude %q: %w", glob, err)
		}
	}

	return nil
}

// files lists what one folder uploads, as fly does: what git tracks and has not deleted plus what it would add, unless asked for everything or the folder is in no repository.
func (f InputFlags) files(ctx context.Context, dir string) ([]string, error) {
	listed, inGit := gitFiles(ctx, dir)
	if f.IncludeIgnored || !inGit {
		var err error

		listed, err = walkFiles(dir)
		if err != nil {
			return nil, err
		}
	}

	kept := listed[:0]

	for _, name := range listed {
		if !excluded(name, f.Exclude) {
			kept = append(kept, name)
		}
	}

	slices.Sort(kept)

	return slices.Compact(kept), nil
}

// gitFiles is fly's getGitFiles: tracked minus deleted, plus untracked that .gitignore and friends do not exclude. False when dir is in no repository or git is not here.
func gitFiles(ctx context.Context, dir string) ([]string, bool) {
	tracked, err := gitLS(ctx, dir)
	if err != nil {
		return nil, false
	}

	deleted, err := gitLS(ctx, dir, "--deleted")
	if err != nil {
		return nil, false
	}

	untracked, err := gitLS(ctx, dir, "--others", "--exclude-standard")
	if err != nil {
		return nil, false
	}

	files := slices.DeleteFunc(tracked, func(name string) bool { return slices.Contains(deleted, name) })

	return append(files, untracked...), true
}

func gitLS(ctx context.Context, dir string, args ...string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"ls-files", "-z"}, args...)...) //nolint:gosec // every caller passes constant flags; dir is where it runs, not an argument
	cmd.Dir = dir

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w", dir, err)
	}

	var names []string

	for name := range strings.SplitSeq(string(out), "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}

	return names, nil
}

// walkFiles is every file and link under dir; an empty directory is not sent, as git does not track one.
func walkFiles(dir string) ([]string, error) {
	var names []string

	err := filepath.WalkDir(dir, func(full string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}

		rel, err := filepath.Rel(dir, full)
		if err != nil {
			return err //nolint:wrapcheck // named below
		}

		names = append(names, filepath.ToSlash(rel))

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", dir, err)
	}

	return names, nil
}

// excluded matches a glob against the path, each directory above it, and its name, so `node_modules` leaves the whole tree out and `*.log` every log.
func excluded(name string, globs []string) bool {
	for _, glob := range globs {
		if ok, _ := path.Match(glob, path.Base(name)); ok {
			return true
		}

		for prefix := name; prefix != "."; prefix = path.Dir(prefix) {
			if ok, _ := path.Match(glob, prefix); ok {
				return true
			}
		}
	}

	return false
}

// uploadBody writes the one-off as a multipart body to a temporary file, so a folder that cannot be packed is reported before the daemon hears anything.
func (f InputFlags) uploadBody(ctx context.Context, req web.OneOffRequest) (*os.File, string, error) {
	body, err := os.CreateTemp("", "steps-one-off-*")
	if err != nil {
		return nil, "", fmt.Errorf("could not stage the upload: %w", err)
	}

	contentType, err := f.writeBody(ctx, body, req)
	if err == nil {
		_, err = body.Seek(0, io.SeekStart)
		if err != nil {
			err = fmt.Errorf("could not stage the upload: %w", err)
		}
	}

	if err != nil {
		discardBody(body)

		return nil, "", err
	}

	return body, contentType, nil
}

func discardBody(body *os.File) {
	_ = body.Close()
	_ = os.Remove(body.Name())
}

func (f InputFlags) writeBody(ctx context.Context, w io.Writer, req web.OneOffRequest) (string, error) {
	form := multipart.NewWriter(w)

	part, err := form.CreateFormField("request")
	if err != nil {
		return "", fmt.Errorf("could not stage the upload: %w", err)
	}

	err = json.NewEncoder(part).Encode(req)
	if err != nil {
		return "", fmt.Errorf("could not encode the configuration: %w", err)
	}

	for _, get := range req.Inputs {
		dir := f.Input[get]

		names, err := f.files(ctx, dir)
		if err != nil {
			return "", fmt.Errorf("--input %s: %w", get, err)
		}

		part, err := form.CreateFormFile(uploadPart+get, get+".tar.zst")
		if err != nil {
			return "", fmt.Errorf("could not stage the upload: %w", err)
		}

		err = compress.Pack(part, true, func(w io.Writer) error { return wire.PackFiles(w, dir, names) })
		if err != nil {
			return "", fmt.Errorf("--input %s: %w", get, err)
		}
	}

	err = form.Close()
	if err != nil {
		return "", fmt.Errorf("could not stage the upload: %w", err)
	}

	return form.FormDataContentType(), nil
}

// uploadPart is web's form-name prefix for a folder, which a CLI and daemon of one build agree on.
const uploadPart = "input:"

// oneOffUpload is oneOff with folders, sent without the verbs' 30-second bound: a gigabyte takes as long as the link takes, and Ctrl-C still ends it.
func (c *daemonClient) oneOffUpload(ctx context.Context, body io.Reader, contentType string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.target+"/api/one-offs", body)
	if err != nil {
		return 0, nil, fmt.Errorf("could not build the request: %w", err)
	}

	req.Header.Set("Content-Type", contentType)

	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}

	unbounded := *c.http
	unbounded.Timeout = 0

	resp, err := unbounded.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("could not reach the steps daemon at %s: %w (start one with: steps web)", c.target, err)
	}

	defer func() { _ = resp.Body.Close() }()

	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("could not read the daemon's answer: %w", err)
	}

	return resp.StatusCode, answer, nil
}

// uploadsRoot holds each one-off's folders as <one-off>/<get>/<digest>, beside its build trees, so a restart serves an interrupted one-off with what it was sent.
func (d *daemon) uploadsRoot() string {
	return filepath.Join(d.exec.workspace(daemonWorkspaceRoot(d.state)).Root, "one-off-inputs")
}

// receiveUploads reads every folder a one-off carries before anything is recorded, refusing the lot past --one-off-input-max-bytes, so a refused upload starts no run.
func (d *daemon) receiveUploads(name string, req web.OneOffRequest) error {
	if req.Uploads != nil {
		return d.readUploads(filepath.Join(d.uploadsRoot(), name), req)
	}

	if len(req.Inputs) > 0 {
		return fmt.Errorf("%w: the request names inputs %v but carries no folders", web.ErrRefused, req.Inputs)
	}

	return nil
}

func (d *daemon) readUploads(root string, req web.OneOffRequest) (err error) {
	defer func() {
		if err != nil {
			_ = os.RemoveAll(root)
		}
	}()

	left := d.oneOffInputMax
	received := map[string]bool{}

	for {
		get, archive, err := req.Uploads.Next()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return fmt.Errorf("could not read the uploads: %w", err)
		}

		if !slices.Contains(req.Inputs, get) || received[get] {
			return fmt.Errorf("%w: a folder for %q, which the request does not name once", web.ErrRefused, get)
		}

		received[get] = true

		used, err := d.receiveUpload(filepath.Join(root, get), archive, left)
		if err != nil {
			return d.uploadRefused(get, err)
		}

		left -= used
	}

	if len(received) != len(req.Inputs) {
		return fmt.Errorf("%w: the request names inputs %v and carries folders for %v", web.ErrRefused, req.Inputs, slices.Sorted(maps.Keys(received)))
	}

	return nil
}

func (d *daemon) uploadRefused(get string, err error) error {
	if errors.Is(err, wire.ErrTreeTooLarge) {
		return fmt.Errorf("%w: the folders sent unpack to more than %d bytes, this daemon's --one-off-input-max-bytes", web.ErrRefused, d.oneOffInputMax)
	}

	return fmt.Errorf("%w: --input %s: %w", web.ErrRefused, get, err)
}

// tarTrailerSlack is what is read past a tar's end so its padding counts in the digest; a stream that goes on past it is not a tar this CLI wrote.
const tarTrailerSlack = 64 << 10

// receiveUpload unpacks one folder into dir/<digest>, the digest being the unpacked tar's own, and answers the bytes its files took.
func (d *daemon) receiveUpload(dir string, archive io.Reader, left int64) (int64, error) {
	partial := filepath.Join(dir, ".partial")

	err := os.MkdirAll(partial, 0o750)
	if err != nil {
		return 0, fmt.Errorf("could not stage the folder: %w", err)
	}

	limits := wire.UnpackOptions{MaxBytes: left, MaxEntries: wire.FetchLimits().MaxEntries}
	if d.oneOffInputMax <= 0 {
		limits.MaxBytes = 0
	} else if left <= 0 {
		return 0, wire.ErrTreeTooLarge
	}

	digest := sha256.New()

	err = compress.Unpack(archive, true, func(r io.Reader) error {
		tee := io.TeeReader(r, digest)

		err := wire.UnpackFetchedTree(tee, partial, limits)
		if err != nil {
			return err //nolint:wrapcheck // the caller says which folder
		}

		_, err = io.CopyN(io.Discard, tee, tarTrailerSlack)
		if errors.Is(err, io.EOF) {
			return nil
		}

		return errors.Join(err, errors.New("the folder's archive goes on past its end"))
	})
	if err != nil {
		return 0, err //nolint:wrapcheck // as above
	}

	used, err := treeBytes(partial)
	if err != nil {
		return 0, err
	}

	err = os.Rename(partial, filepath.Join(dir, hex.EncodeToString(digest.Sum(nil))))
	if err != nil {
		return 0, fmt.Errorf("could not keep the folder: %w", err)
	}

	return used, nil
}

func treeBytes(dir string) (int64, error) {
	var total int64

	err := filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}

		info, err := entry.Info()
		if err != nil {
			return err //nolint:wrapcheck // named below
		}

		total += info.Size()

		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("could not measure the folder: %w", err)
	}

	return total, nil
}

// uploadsFor reads back the folders a one-off was sent, as RunJob takes them; a folder the disk lost is left out, and its get fetches as written.
func (d *daemon) uploadsFor(ctx context.Context, name string) map[string]pipeline.Upload {
	root := filepath.Join(d.uploadsRoot(), name)

	gets, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	uploads := map[string]pipeline.Upload{}

	for _, get := range gets {
		trees, err := os.ReadDir(filepath.Join(root, get.Name()))
		if err != nil || len(trees) != 1 || !trees[0].IsDir() {
			slog.WarnContext(ctx, "web.one_off.upload_unreadable", "pipeline", name, "get", get.Name())

			continue
		}

		uploads[get.Name()] = pipeline.Upload{Dir: filepath.Join(root, get.Name(), trees[0].Name()), Digest: trees[0].Name()}
	}

	return uploads
}

// forgetUploads removes a one-off's folders with it.
func (d *daemon) forgetUploads(ctx context.Context, name string) {
	err := os.RemoveAll(filepath.Join(d.uploadsRoot(), name))
	if err != nil {
		slog.WarnContext(ctx, "web.one_off.uploads_unremoved", "pipeline", name, "error", err)
	}
}
