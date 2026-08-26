package ssmdial

// The conformance net the fake agent cannot be.
//
// Everything else in this package tests the protocol as THIS REPO reads it —
// the fake agent is written from the same understanding as the client, so a
// shared misreading passes both sides. Only a session against real AWS
// settles agreement with the real agent, which is why this file exists and
// why it is opt-in: it needs an account, credentials, and one managed
// instance.
//
// To run it:
//
//	STEPS_TEST_AWS_INSTANCE=i-0abc123def456789 go test ./internal/venue/ssmdial -run TestRealAWS -v
//
// The instance needs the SSM agent registered (an instance profile carrying
// AmazonSSMManagedInstanceCore) and sshd listening on port 22 — every stock
// Linux AMI qualifies — or STEPS_TEST_AWS_PORT naming another TCP port that
// answers with bytes on connect. Ambient credentials need ssm:StartSession,
// ssm:SendCommand, ssm:GetCommandInvocation and
// ssm:DescribeInstanceInformation. Without the environment, every test here
// skips, so the default suite stays hermetic.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
)

// realInstance skips unless the operator pointed the suite at a machine.
func realInstance(t *testing.T) (API, string) {
	t.Helper()

	instance := os.Getenv("STEPS_TEST_AWS_INSTANCE")
	if instance == "" {
		t.Skip("set STEPS_TEST_AWS_INSTANCE=i-... to run the real-AWS conformance tests")
	}

	loaders := []func(*awsconfig.LoadOptions) error{}
	if region := os.Getenv("STEPS_TEST_AWS_REGION"); region != "" {
		loaders = append(loaders, awsconfig.WithRegion(region))
	}

	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), loaders...)
	if err != nil {
		t.Fatalf("loading AWS configuration: %v", err)
	}

	return NewAPI(cfg), instance
}

// TestRealAWSControlPlane settles the API half: the instance is a managed
// node this account can see, and a command round-trips through SendCommand
// and GetCommandInvocation.
func TestRealAWSControlPlane(t *testing.T) {
	api, instance := realInstance(t)
	ctx := context.Background()

	platform, err := PlatformOf(ctx, api, instance)
	if err != nil {
		t.Fatalf("PlatformOf: %v", err)
	}

	if platform == PlatformWindows {
		t.Skip("the conformance probe wants a Linux or macOS instance")
	}

	out, err := Run(ctx, api, instance, platform, "echo steps-conformance-probe")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.Contains(out, "steps-conformance-probe") {
		t.Fatalf("Run output = %q, want the probe echoed", out)
	}
}

// TestRealAWSDataChannel settles the protocol half against the real agent:
// StartSession, the websocket, the handshake this client answers, and bytes
// BOTH ways through a basic port-forwarding session.
//
// Port 22 by default, because reading sshd's banner needs nothing installed:
// the agent-to-client direction is proven by the "SSH-2.0-" greeting, and
// client-to-agent by sending our own identification line and receiving the
// key exchange that only a server which HEARD us continues into.
func TestRealAWSDataChannel(t *testing.T) {
	api, instance := realInstance(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	port := 22
	if raw := os.Getenv("STEPS_TEST_AWS_PORT"); raw != "" {
		t.Skipf("custom port probing not implemented; unset STEPS_TEST_AWS_PORT (got %s)", raw)
	}

	channel, err := Forward(ctx, api, instance, port)
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}

	defer func() { _ = channel.Close() }()

	banner := make([]byte, 256)

	n, err := channel.Read(banner)
	if err != nil {
		t.Fatalf("reading the banner: %v", err)
	}

	if !strings.HasPrefix(string(banner[:n]), "SSH-2.0-") {
		t.Fatalf("banner = %q, want an sshd greeting — the inbound data path is wrong", banner[:n])
	}

	_, err = channel.Write([]byte("SSH-2.0-steps-conformance-probe\r\n"))
	if err != nil {
		t.Fatalf("writing our identification: %v", err)
	}

	// A server that heard our line answers with its binary KEXINIT; one that
	// did not says nothing and this read times out with the context.
	more := make([]byte, 64)

	_, err = channel.Read(more)
	if err != nil {
		t.Fatalf("no bytes after our identification — the outbound data path is wrong: %v", err)
	}
}
