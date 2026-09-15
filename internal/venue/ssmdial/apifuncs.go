package ssmdial

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// apiFuncs holds method values, not the client: a *ssm.Client in an interface keeps every SSM operation linked (~4MB).
type apiFuncs struct {
	startSession func(context.Context, *ssm.StartSessionInput, ...func(*ssm.Options)) (*ssm.StartSessionOutput, error)
	sendCommand  func(context.Context, *ssm.SendCommandInput, ...func(*ssm.Options)) (*ssm.SendCommandOutput, error)
	invocation   func(context.Context, *ssm.GetCommandInvocationInput, ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error)
	describe     func(context.Context, *ssm.DescribeInstanceInformationInput, ...func(*ssm.Options)) (*ssm.DescribeInstanceInformationOutput, error)
}

func newAPIFuncs(client *ssm.Client) apiFuncs {
	return apiFuncs{
		startSession: client.StartSession,
		sendCommand:  client.SendCommand,
		invocation:   client.GetCommandInvocation,
		describe:     client.DescribeInstanceInformation,
	}
}

func (f apiFuncs) StartSession(ctx context.Context, in *ssm.StartSessionInput, opts ...func(*ssm.Options)) (*ssm.StartSessionOutput, error) {
	return f.startSession(ctx, in, opts...)
}

func (f apiFuncs) SendCommand(ctx context.Context, in *ssm.SendCommandInput, opts ...func(*ssm.Options)) (*ssm.SendCommandOutput, error) {
	return f.sendCommand(ctx, in, opts...)
}

func (f apiFuncs) GetCommandInvocation(ctx context.Context, in *ssm.GetCommandInvocationInput, opts ...func(*ssm.Options)) (*ssm.GetCommandInvocationOutput, error) {
	return f.invocation(ctx, in, opts...)
}

func (f apiFuncs) DescribeInstanceInformation(ctx context.Context, in *ssm.DescribeInstanceInformationInput, opts ...func(*ssm.Options)) (*ssm.DescribeInstanceInformationOutput, error) {
	return f.describe(ctx, in, opts...)
}
