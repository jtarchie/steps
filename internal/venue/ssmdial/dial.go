package ssmdial

// Starting a session: the SSM control-plane call that mints a stream URL, and
// the command call that gets a shim running on the far end.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// portForwardDocument is the AWS-managed document that forwards a local
// connection to a port on the managed node.
//
// Deliberately not AWS-StartSSHSession: that tunnels to sshd, which would
// re-import the dependency this dialer exists to remove — an sshd, host keys
// and authorized_keys on every worker, and on Windows a feature-on-demand
// enterprise images strip.
const portForwardDocument = "AWS-StartPortForwardingSession"

// runShellDocument and runPowerShellDocument run the bootstrap that starts a
// shim. Which one is chosen by the node's platform, which SSM reports.
const (
	runShellDocument      = "AWS-RunShellScript"
	runPowerShellDocument = "AWS-RunPowerShellScript"
)

// API is the slice of the SSM API this package uses, declared so a test can
// stand in for it without a network.
type API interface {
	StartSession(ctx context.Context, in *ssm.StartSessionInput, opts ...func(*ssm.Options)) (*ssm.StartSessionOutput, error)
	SendCommand(ctx context.Context, in *ssm.SendCommandInput, opts ...func(*ssm.Options)) (*ssm.SendCommandOutput, error)
	GetCommandInvocation(ctx context.Context, in *ssm.GetCommandInvocationInput, opts ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error)
	DescribeInstanceInformation(ctx context.Context, in *ssm.DescribeInstanceInformationInput, opts ...func(*ssm.Options)) (*ssm.DescribeInstanceInformationOutput, error)
}

// NewAPI builds the real client.
func NewAPI(cfg aws.Config) API { return ssm.NewFromConfig(cfg) }

// Forward opens a session forwarding to port on the instance, returning it as
// a byte pipe.
func Forward(ctx context.Context, api API, instance string, port int) (*Channel, error) {
	out, err := api.StartSession(ctx, &ssm.StartSessionInput{
		Target:       aws.String(instance),
		DocumentName: aws.String(portForwardDocument),
		Parameters: map[string][]string{
			"portNumber":      {strconv.Itoa(port)},
			"localPortNumber": {"0"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("starting an SSM session with %s: %w", instance, err)
	}

	if out.StreamUrl == nil || out.TokenValue == nil {
		return nil, fmt.Errorf("starting an SSM session with %s: the service returned no stream", instance) //nolint:err113 // names the instance, which is the actionable part
	}

	channel, err := Open(ctx, *out.StreamUrl, *out.TokenValue)
	if err != nil {
		return nil, err
	}

	return channel, nil
}

// Platform is what an instance runs, which decides both how to bootstrap it
// and whether steps can use it at all.
type Platform string

const (
	// PlatformWindows is reported for a Windows managed node.
	PlatformWindows Platform = "Windows"
	// PlatformLinux covers Linux and macOS nodes, which bootstrap the same
	// way — a shell script.
	PlatformLinux Platform = "Linux"
)

// PlatformOf asks SSM what an instance is, and confirms in the same call that
// SSM can reach it. An instance whose agent is not registered fails here,
// naming the instance, rather than as a session that never establishes.
func PlatformOf(ctx context.Context, api API, instance string) (Platform, error) {
	out, err := api.DescribeInstanceInformation(ctx, &ssm.DescribeInstanceInformationInput{
		Filters: []types.InstanceInformationStringFilter{
			{Key: aws.String("InstanceIds"), Values: []string{instance}},
		},
	})
	if err != nil {
		return "", fmt.Errorf("asking SSM about %s: %w", instance, err)
	}

	if len(out.InstanceInformationList) == 0 {
		return "", fmt.Errorf("%w: %s is not a managed node SSM can reach — check the instance profile carries AmazonSSMManagedInstanceCore and the agent is running",
			ErrNotManaged, instance)
	}

	if out.InstanceInformationList[0].PlatformType == types.PlatformTypeWindows {
		return PlatformWindows, nil
	}

	return PlatformLinux, nil
}

// ErrNotManaged is an instance SSM does not know about.
//
// Exported because the answer is ambiguous in time, and only the caller knows
// which case it is in: a machine acquired seconds ago has an agent that has
// simply not registered yet, while one that has been running for hours is
// misconfigured. This package cannot tell those apart, so it names the
// condition and lets the venue decide whether to wait.
var ErrNotManaged = errormsg("instance is not registered with SSM")

// errormsg is a tiny error type, so the sentinels in this file read as values
// rather than as a var block far from their use.
type errormsg string

func (e errormsg) Error() string { return string(e) }

// commandTimeout bounds a bootstrap command. Generous: it covers a cold
// instance downloading a ~56MB binary.
const commandTimeout = 5 * time.Minute

// commandPoll is how often a running bootstrap is asked whether it finished.
const commandPoll = 2 * time.Second

// Run executes a bootstrap script on the instance and waits for it, returning
// what it printed. A nonzero exit is an error carrying the command's own
// stderr, because that is the only account of a bootstrap that failed on a
// machine nobody is logged in to.
func Run(ctx context.Context, api API, instance string, platform Platform, script string) (string, error) {
	document := runShellDocument
	if platform == PlatformWindows {
		document = runPowerShellDocument
	}

	sent, err := api.SendCommand(ctx, &ssm.SendCommandInput{
		InstanceIds:  []string{instance},
		DocumentName: aws.String(document),
		Parameters:   map[string][]string{"commands": {script}},
	})
	if err != nil {
		return "", fmt.Errorf("running a command on %s: %w", instance, err)
	}

	if sent.Command == nil || sent.Command.CommandId == nil {
		return "", fmt.Errorf("running a command on %s: the service returned no command id", instance) //nolint:err113 // names the instance, which is the actionable part
	}

	return waitForCommand(ctx, api, instance, *sent.Command.CommandId)
}

// waitForCommand polls one invocation to a terminal state.
func waitForCommand(ctx context.Context, api API, instance, commandID string) (string, error) {
	deadline, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	ticker := time.NewTicker(commandPoll)
	defer ticker.Stop()

	for {
		out, err := api.GetCommandInvocation(deadline, &ssm.GetCommandInvocationInput{
			CommandId:  aws.String(commandID),
			InstanceId: aws.String(instance),
		})
		// An invocation is briefly unknown right after SendCommand returns, so
		// an error here is only fatal once the deadline has passed.
		if err != nil && deadline.Err() != nil {
			return "", fmt.Errorf("waiting for a command on %s: %w", instance, err)
		}

		if err == nil {
			switch out.Status {
			case types.CommandInvocationStatusSuccess:
				return aws.ToString(out.StandardOutputContent), nil
			case types.CommandInvocationStatusCancelled,
				types.CommandInvocationStatusFailed,
				types.CommandInvocationStatusTimedOut:
				return "", fmt.Errorf("%w on %s: %s: %s", errCommandFailed, instance, out.Status,
					firstNonEmpty(aws.ToString(out.StandardErrorContent), aws.ToString(out.StandardOutputContent)))
			case types.CommandInvocationStatusDelayed,
				types.CommandInvocationStatusInProgress,
				types.CommandInvocationStatusPending,
				types.CommandInvocationStatusCancelling:
			}
		}

		select {
		case <-ticker.C:
		case <-deadline.Done():
			return "", fmt.Errorf("waiting for a command on %s: %w", instance, deadline.Err())
		}
	}
}

// errCommandFailed is a bootstrap that ran and did not succeed.
var errCommandFailed = errormsg("the bootstrap command failed")

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}
