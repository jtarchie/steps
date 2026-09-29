package venue

// The shims this steps carries for workers of other platforms.
//
// `task shims` cross-compiles cmd/steps-shim into shims/ before `task build`
// embeds it; a plain `go build` embeds whatever the directory last held, which
// on a fresh checkout is nothing, and a worker is then pushed this process
// instead (see resolveShim).

import (
	"embed"
	"io"
	"io/fs"
	"slices"
	"strings"
	"sync"

	"github.com/jtarchie/steps/internal/shim"
)

//go:embed all:shims
var shimsFS embed.FS

// shimsDir is where the embedded shims sit inside embeddedShims.
const shimsDir = "shims"

// embeddedShims is what resolveShim picks from.
//
//nolint:gochecknoglobals // test seam over the embedded shims
var embeddedShims fs.FS = shimsFS

// shimName is the file an embedded shim for a platform is filed under.
func shimName(goos, goarch string) string {
	return "steps-shim-" + goos + "-" + goarch
}

// embeddedSize is the size of the shim for a platform, or 0 when none is
// embedded. An empty file — a build interrupted mid-write — counts as none:
// pushing it would put a zero-byte "binary" on the worker. A stat, not a
// read: embed.FS hands out a copy, megabytes each.
func embeddedSize(goos, goarch string) int64 {
	info, err := fs.Stat(embeddedShims, shimsDir+"/"+shimName(goos, goarch))
	if err != nil {
		return 0
	}

	return info.Size()
}

// embeddedBuilds memoizes each embedded shim's content hash by name, for the
// same reason localBuilds does: a session is dialled per step. withShims
// clears it along with the seam.
//
//nolint:gochecknoglobals // a cache over immutable embedded files
var embeddedBuilds sync.Map

// embeddedSource is the shim for a platform as a push source, or false when
// none is embedded.
func embeddedSource(goos, goarch, platform string) (shimSource, bool) {
	size := embeddedSize(goos, goarch)
	if size == 0 {
		return shimSource{}, false
	}

	name := shimName(goos, goarch)
	file := shimsDir + "/" + name

	build, ok := embeddedBuilds.Load(name)
	if !ok {
		binary, err := fs.ReadFile(embeddedShims, file)
		if err != nil {
			return shimSource{}, false
		}

		build, _ = embeddedBuilds.LoadOrStore(name, shim.BuildOfBytes(binary))
	}

	return shimSource{
		kind:     kindEmbedded,
		name:     name,
		build:    build.(string), //nolint:forcetypeassert // this map holds one type
		size:     size,
		open:     func() (io.ReadCloser, error) { return embeddedShims.Open(file) },
		platform: platform,
	}, true
}

// embeddedPlatforms lists the platforms a shim is embedded for, as goos/goarch.
func embeddedPlatforms() []string {
	entries, err := fs.ReadDir(embeddedShims, shimsDir)
	if err != nil {
		return nil
	}

	var platforms []string

	for _, entry := range entries {
		rest, ok := strings.CutPrefix(entry.Name(), "steps-shim-")
		if !ok {
			continue
		}

		goos, goarch, ok := strings.Cut(rest, "-")
		if ok && embeddedSize(goos, goarch) > 0 {
			platforms = append(platforms, goos+"/"+goarch)
		}
	}

	slices.Sort(platforms)

	return platforms
}

// parsePlatform reads `uname -sm` into Go's names for it. A fixed table, so a
// worker's own words never become a path into embeddedShims.
func parsePlatform(uname string) (goos, goarch string, ok bool) {
	fields := strings.Fields(uname)
	if len(fields) != 2 {
		return "", "", false
	}

	switch fields[0] {
	case "Linux":
		goos = "linux"
	case "Darwin":
		goos = "darwin"
	default:
		return "", "", false
	}

	switch fields[1] {
	case "x86_64", "amd64":
		goarch = "amd64"
	case "aarch64", "arm64":
		goarch = "arm64"
	default:
		return "", "", false
	}

	return goos, goarch, true
}
