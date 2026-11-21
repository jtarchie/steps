package dockerapi

import (
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/moby/moby/client"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// untracedKey marks a request whose span would only mislead: SettleFor's wait ends on its own deadline by design, which a span records as an error on every healthy container.
type untracedKey struct{}

// connect builds every engine client here, so each one names and filters its spans the same way.
func connect(opts ...client.Opt) (*client.Client, error) {
	traced := client.WithTraceOptions(
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + routeOf(r.URL.Path)
		}),
		// The ping is the client negotiating its API version ahead of a first call, and says nothing the call's own span does not.
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.Context().Value(untracedKey{}) == nil && !strings.HasSuffix(r.URL.Path, "/_ping")
		}),
	)

	//nolint:wrapcheck // New and NewDialer name the daemon, which this cannot.
	return client.New(append([]client.Opt{traced}, opts...)...)
}

var apiVersion = regexp.MustCompile(`^/v\d+\.\d+`)

// collections are the engine routes whose second segment names one thing, beside the words that segment can be instead.
var collections = map[string]struct {
	placeholder string
	verbs       []string
}{
	"containers": {"{id}", []string{"json", "create", "prune"}},
	"exec":       {"{id}", nil},
	"networks":   {"{id}", []string{"create", "prune"}},
	"volumes":    {"{name}", []string{"create", "prune"}},
	"images":     {"{name}", []string{"json", "create", "load", "prune", "search", "get"}},
}

// imageActions end an image's route; everything between them and /images is the reference, slashes included.
var imageActions = []string{"json", "history", "push", "tag", "get"}

// routeOf is an engine path with its ids taken out, so a span is named by what was asked rather than of which container: a backend groups by name, and the raw path was a new name per container, volume and image.
func routeOf(path string) string {
	segments := strings.Split(strings.TrimPrefix(apiVersion.ReplaceAllString(path, ""), "/"), "/")

	collection, ok := collections[segments[0]]
	if !ok || len(segments) < 2 || (len(segments) == 2 && slices.Contains(collection.verbs, segments[1])) {
		return "/" + strings.Join(segments, "/")
	}

	rest := segments[2:]
	if segments[0] == "images" {
		rest = nil
		if last := segments[len(segments)-1]; len(segments) > 2 && slices.Contains(imageActions, last) {
			rest = []string{last}
		}
	}

	return "/" + strings.Join(append([]string{segments[0], collection.placeholder}, rest...), "/")
}
