package venue

// The cloud's own word on a machine whose connection dropped with no reclaim notice heard: GCE can close the IAP relay before its metadata says anything (measured on the steps#225 bench), so the notice cannot be the only evidence a machine was taken.

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"github.com/jtarchie/steps/internal/shell"
)

//nolint:gochecknoglobals // test seams: how long a status that lags the drop is waited for, and how often it is asked
var (
	goneWindow = 20 * time.Second
	goneEvery  = 4 * time.Second
)

// evictionOf classifies a command's error: an eviction when the worker heard its own reclaim notice or, for a session that was up, its cloud says the machine is going away. Either is remembered as the session's reclaim, so the lease lets the machine go.
func (s *plusSession) evictionOf(ctx context.Context, err error, up bool) error {
	reason, reclaimed := "", false
	if heard := s.drain.Load(); heard != nil {
		reason, reclaimed = *heard, true
	}

	// Not on a command that answered for itself, nor on a step being stopped: neither is the machine's doing.
	if up && !reclaimed && err != nil && !shell.IsExitError(err) && ctx.Err() == nil {
		reason, reclaimed = goneAway(ctx, s.worker)
		if reclaimed {
			s.drain.Store(&reason)
		}
	}

	return asEvictionOf(err, reason, reclaimed)
}

// goneAway asks the worker's cloud whether its instance is being taken away, waiting out a status that has not caught up with the drop yet. Anything but the cloud's clear word is no, so a blip on a healthy machine stays the error it was.
func goneAway(ctx context.Context, worker Worker) (string, bool) {
	var ask func(context.Context, Worker) (string, bool)

	switch worker.Scheme {
	case SchemeGCP:
		ask = gceGoneAway
	case SchemeAWS:
		ask = ec2GoneAway
	case SchemeLocal, SchemeSSH, SchemeDockerSSH, SchemeDO:
		// No control plane to ask: DO has no spot capacity, and the rest are machines steps never acquired.
		return "", false
	}

	if worker.Instance == "" {
		return "", false
	}

	ctx, cancel := context.WithTimeout(ctx, goneWindow)
	defer cancel()

	for {
		reason, gone := ask(ctx, worker)
		if gone {
			return reason, true
		}

		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(goneEvery):
		}
	}
}

func gceGoneAway(ctx context.Context, worker Worker) (string, bool) {
	api, err := gceFor(ctx, worker)
	if err != nil {
		return "", false
	}

	project, zone, err := gcpLocation(ctx, worker)
	if err != nil {
		return "", false
	}

	status, err := api.Status(ctx, project, zone, worker.Instance)
	if errors.Is(err, errGCENotFound) {
		return "GCE no longer has the instance", true
	}

	switch status {
	case "STOPPING", "TERMINATED", "SUSPENDING", "SUSPENDED":
		return "GCE reports the instance " + status, true
	}

	return "", false
}

func ec2GoneAway(ctx context.Context, worker Worker) (string, bool) {
	api, err := ec2For(ctx, worker)
	if err != nil {
		return "", false
	}

	out, err := api.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{worker.Instance}})

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode() == "InvalidInstanceID.NotFound" {
		return "EC2 no longer has the instance", true
	}

	if err != nil {
		return "", false
	}

	state := describedState(out)

	switch state {
	case ec2types.InstanceStateNameShuttingDown, ec2types.InstanceStateNameTerminated,
		ec2types.InstanceStateNameStopping, ec2types.InstanceStateNameStopped:
		return "EC2 reports the instance " + string(state), true
	case ec2types.InstanceStateNamePending, ec2types.InstanceStateNameRunning:
	}

	return "", false
}

// describedState is the one instance's state a describe by id answered, empty when it answered nothing.
func describedState(out *ec2.DescribeInstancesOutput) ec2types.InstanceStateName {
	if out == nil || len(out.Reservations) == 0 || len(out.Reservations[0].Instances) == 0 || out.Reservations[0].Instances[0].State == nil {
		return ""
	}

	return out.Reservations[0].Instances[0].State.Name
}
