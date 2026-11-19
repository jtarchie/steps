package config

// on_pending's ((.:pending.*)): what an announcement may read, and where.

import (
	"fmt"
	"slices"
	"strings"
)

// PendingCapture is the ((.:name)) an on_pending body reads its park through.
const PendingCapture = "pending"

// PendingFields are the fields of ((.:pending.*)), each also handed to a command as STEPS_PENDING_<FIELD>.
var PendingFields = []string{"id", "kind", "job", "message", "options", "command", "url", "expires_at"} //nolint:gochecknoglobals // static, read-only

// untrustedPendingFields can carry text a model wrote (an ask_user question): data for params: and messages:, never text substituted into a command.
var untrustedPendingFields = []string{"message", "options"} //nolint:gochecknoglobals // static, read-only

// PendingEnvName is the variable a field reaches a command as.
func PendingEnvName(field string) string {
	return "STEPS_PENDING_" + strings.ToUpper(field)
}

func checkPendingRef(label, field string, ref LocalRef, produced varSet) error {
	if !produced[PendingCapture] {
		return fmt.Errorf("%s: %s references %s, which is known only inside an on_pending body", label, field, ref)
	}

	if len(ref.Fields) > 1 || (len(ref.Fields) == 1 && !slices.Contains(PendingFields, ref.Fields[0])) {
		return fmt.Errorf("%s: %s references %s; a park has %s", label, field, ref, strings.Join(PendingFields, ", "))
	}

	if field == "params" || strings.HasPrefix(field, "messages[") {
		return nil
	}

	if len(ref.Fields) == 0 {
		return fmt.Errorf("%s: %s references %s whole, which holds text a model may have written and would be substituted into a command; name a field, or pass it through params", label, field, ref)
	}

	if slices.Contains(untrustedPendingFields, ref.Fields[0]) {
		return fmt.Errorf("%s: %s references %s, which may hold text a model wrote and would be substituted into a command; read $%s instead, or pass it through params",
			label, field, ref, PendingEnvName(ref.Fields[0]))
	}

	return nil
}
