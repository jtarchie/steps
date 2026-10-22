package store

import "testing"

// The stored strings are a contract with every database already written and with the SQL literals in both drivers, so a rename of a constant must not move them.
func TestStatusConstantsKeepTheirStoredStrings(t *testing.T) {
	t.Parallel()

	for _, c := range []struct{ got, want string }{
		{string(QueuePending), "pending"},
		{string(QueueRunning), "running"},
		{string(QueueSucceeded), "succeeded"},
		{string(QueueFailed), "failed"},
		{string(QueueAborted), "aborted"},
		{string(QueueSkipped), "skipped"},
		{string(NodeRunning), "running"},
		{string(NodeSucceeded), "succeeded"},
		{string(NodeFailed), "failed"},
		{string(NodeErrored), "errored"},
		{string(NodeAborted), "aborted"},
		{string(TokenAccess), "access"},
		{string(TokenRefresh), "refresh"},
	} {
		if c.got != c.want {
			t.Errorf("constant is %q, stored form is %q", c.got, c.want)
		}
	}
}
