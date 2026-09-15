package venue

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
)

// ec2Funcs holds method values, not the client: a *ec2.Client in an interface keeps all ~600 EC2 operations linked (~20MB).
type ec2Funcs struct {
	start     func(context.Context, *ec2.StartInstancesInput, ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error)
	stop      func(context.Context, *ec2.StopInstancesInput, ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error)
	terminate func(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	describe  func(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	fleet     func(context.Context, *ec2.CreateFleetInput, ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error)
}

func newEC2Funcs(client *ec2.Client) ec2Funcs {
	return ec2Funcs{
		start:     client.StartInstances,
		stop:      client.StopInstances,
		terminate: client.TerminateInstances,
		describe:  client.DescribeInstances,
		fleet:     client.CreateFleet,
	}
}

func (f ec2Funcs) StartInstances(ctx context.Context, in *ec2.StartInstancesInput, opts ...func(*ec2.Options)) (*ec2.StartInstancesOutput, error) {
	return f.start(ctx, in, opts...)
}

func (f ec2Funcs) StopInstances(ctx context.Context, in *ec2.StopInstancesInput, opts ...func(*ec2.Options)) (*ec2.StopInstancesOutput, error) {
	return f.stop(ctx, in, opts...)
}

func (f ec2Funcs) TerminateInstances(ctx context.Context, in *ec2.TerminateInstancesInput, opts ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	return f.terminate(ctx, in, opts...)
}

func (f ec2Funcs) DescribeInstances(ctx context.Context, in *ec2.DescribeInstancesInput, opts ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return f.describe(ctx, in, opts...)
}

func (f ec2Funcs) CreateFleet(ctx context.Context, in *ec2.CreateFleetInput, opts ...func(*ec2.Options)) (*ec2.CreateFleetOutput, error) {
	return f.fleet(ctx, in, opts...)
}
