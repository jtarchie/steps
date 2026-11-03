package agent

// The keys of a tool's result map. The map goes to the model as the tool's JSON response and is read back by the dispatcher, the failure probes and the telemetry layer, so a spelling shared between files lives here; these are wire values, pinned by TestResultKeysAreWireSpelled.
const (
	resultErrorKey     = "error"
	resultExitCodeKey  = "exit_code"
	resultVerdictKey   = "verdict"
	resultNoteKey      = "note"
	resultResultKey    = "result"
	resultContentKey   = "content"
	resultTruncatedKey = "truncated"
	resultMessageKey   = "message"
)

func errorResult(message string) map[string]any {
	return map[string]any{resultErrorKey: message}
}

// stringsArg reads a list-of-strings tool argument. The model's arguments
// arrive as []any through JSON; a Go caller (tests, the bridge) may pass
// []string directly. Anything else — including a bare string where a list was
// asked for — is read as no options rather than guessed at.
func stringsArg(args map[string]any, key string) []string {
	switch v := args[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))

		for _, entry := range v {
			if s, ok := entry.(string); ok && s != "" {
				out = append(out, s)
			}
		}

		return out
	default:
		return nil
	}
}

func stringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)

	return v
}

// intArg reads an integer tool argument. The genai/JSON path decodes a
// model-supplied number as float64; the int case exists only so
// Go-constructed args (tests, sub-agent/verdict plumbing) can pass a plain
// int without going through JSON first. The bool return distinguishes "not
// supplied" from "supplied as 0", which read_file's start_line/end_line
// both need (0 is not a valid line number, but its absence and its
// explicit-zero are different requests).
func intArg(args map[string]any, key string) (int, bool) {
	switch v := args[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	default:
		return 0, false
	}
}
