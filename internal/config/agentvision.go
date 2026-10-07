package config

// Whether a hosted model can be shown an image a tool read.

import "strings"

// visionModels maps the start of a model's own id to whether it accepts image
// input, most specific first. Matched against the LAST path segment of the
// normalized name, so `openrouter/anthropic/claude-sonnet-4.5` and
// `anthropic/claude-sonnet-4-5` are both `claude-sonnet-4-5` here, and a
// prefix rather than a substring because `o3` is inside too many unrelated ids.
//
// Unlisted means no, which is the safe direction to be wrong in: a model
// wrongly believed blind gets an honest description of the image, while one
// wrongly believed sighted gets an image_url part its endpoint answers with a
// 400, failing the step. So an entry is worth adding only for a family whose
// image input is confidently known, and a sibling that is not confidently
// sighted is listed as false ahead of its family.
//
//nolint:gochecknoglobals // static, read-only lookup table
var visionModels = []struct {
	prefix string
	sees   bool
}{
	{"claude-", true},
	// Served text-only at launch; not confidently known to take images since.
	{"gpt-5-3-codex-spark", false},
	{"gpt-5", true},
	{"gpt-4-1", true},
	{"gpt-4o", true},
	{"o3-mini", false},
	{"o3", true},
	{"o4-mini", true},
	{"gemini-", true},
}

// ModelSeesImages reports whether a hosted model is known to accept an image.
// A CLI source is not asked: what its CLI does with an image is the CLI's
// business, not the model name's.
func ModelSeesImages(modelName string) bool {
	name := normalizeModelName(modelName)
	if at := strings.LastIndex(name, "/"); at >= 0 {
		name = name[at+1:]
	}

	for _, entry := range visionModels {
		if strings.HasPrefix(name, entry.prefix) {
			return entry.sees
		}
	}

	return false
}
