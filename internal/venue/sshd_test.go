package venue

import (
	"testing"

	"github.com/jtarchie/steps/internal/testsshd"
)

type testSSHD = testsshd.Server

func newTestSSHD(t *testing.T) *testSSHD { return testsshd.New(t) }

var generateKey = testsshd.GenerateKey
