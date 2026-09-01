package venue

// The gcp:// acquisition ladder — ec2.go's three rungs, for Compute Engine:
//
//	gcp://worker-1           a running instance — dial it
//	gcp://stopped/worker-1   a parked instance — start it, dial it, stop it
//	gcp://launch/template-1  no instance — create one from an instance
//	                         template, dial it, delete it
//
// The instance template owns the ENTIRE machine vocabulary: image, machine
// type, disks, network, service account, provisioning model (spot or
// standard) and its termination action, metadata, labels. steps grows no GCE
// configuration surface at all — including the two knobs aws:// has and this
// scheme deliberately does not: no ?capacity=, because a template DECIDES its
// provisioning model where an EC2 fleet request cannot, and no ?version=,
// because a template is one immutable object rather than a container of
// numbered versions — a different shape is a different template.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
)

// gceAPI is the slice of Compute Engine this package uses, in this package's
// own vocabulary rather than the SDK's, so a test can stand in for it without
// an account.
type gceAPI interface {
	// InsertFromTemplate creates one named instance from an instance
	// template, returning once the create operation has settled enough to
	// carry a real error — a bad template or an exhausted quota must fail
	// here, not as a ten-minute wait for a machine that was never coming.
	InsertFromTemplate(ctx context.Context, project, zone, name, template string) error
	Start(ctx context.Context, project, zone, name string) error
	Stop(ctx context.Context, project, zone, name string) error
	Delete(ctx context.Context, project, zone, name string) error
	// Status is the instance's lifecycle state (RUNNING, PROVISIONING, …),
	// or errGCENotFound for an instance the API denies exists.
	Status(ctx context.Context, project, zone, name string) (string, error)
	// AddSSHKey merges one authorized-key entry into the instance's ssh-keys
	// metadata.
	AddSSHKey(ctx context.Context, project, zone, name, entry string) error
	// GuestAttributes reads one namespace of the instance's guest attributes,
	// empty when nothing is published yet and errGuestAttributesDisabled when
	// the instance does not expose them at all.
	GuestAttributes(ctx context.Context, project, zone, name, path string) (map[string]string, error)
}

// errGCENotFound is an instance the API says does not exist — which for a
// machine created moments ago means "not yet".
var errGCENotFound = errors.New("the instance does not exist")

// errGuestAttributesDisabled is an instance that cannot attest its host keys.
var errGuestAttributesDisabled = errors.New("guest attributes are disabled on the instance")

// gceFor builds the Compute Engine client for a worker, seamed for the same
// reason ec2For is: acquisition is the part no test can exercise without an
// account, so it is the part a test stands in for.
//
//nolint:gochecknoglobals // a test seam for a control plane, documented above
var gceFor = func(ctx context.Context, worker Worker) (gceAPI, error) {
	service, err := compute.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w %q: no GCP credentials (log in with `gcloud auth application-default login`): %w",
			ErrWorker, worker.URL, err)
	}

	return &gceClient{service: service}, nil
}

// gceClient adapts the generated compute client to gceAPI.
type gceClient struct {
	service *compute.Service
}

func (c *gceClient) InsertFromTemplate(ctx context.Context, project, zone, name, template string) error {
	templateURL := "projects/" + project + "/global/instanceTemplates/" + template

	op, err := c.service.Instances.Insert(project, zone, &compute.Instance{Name: name}).
		SourceInstanceTemplate(templateURL).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("creating %s from template %s: %w", name, template, err)
	}

	// Waited out, because the interesting failures are asynchronous: a quota
	// the project has exhausted, a template naming a subnet that does not
	// exist, a spot pool with nothing in it. They all land in the operation,
	// and skipping the wait turns each into a silent ten-minute timeout.
	err = c.awaitZoneOperation(ctx, project, zone, op.Name)
	if err != nil {
		return fmt.Errorf("creating %s from template %s: %w", name, template, err)
	}

	return nil
}

// awaitZoneOperation blocks until a zone operation finishes, and returns what
// it says went wrong.
func (c *gceClient) awaitZoneOperation(ctx context.Context, project, zone, operation string) error {
	for {
		op, err := c.service.ZoneOperations.Wait(project, zone, operation).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("waiting for the operation: %w", err)
		}

		if op.Status != "DONE" {
			// Wait returns after at most two minutes whether or not the
			// operation finished; only the caller's context bounds the total.
			continue
		}

		if op.Error != nil && len(op.Error.Errors) > 0 {
			first := op.Error.Errors[0]

			return fmt.Errorf("the operation failed: %s: %s", first.Code, first.Message)
		}

		return nil
	}
}

func (c *gceClient) Start(ctx context.Context, project, zone, name string) error {
	_, err := c.service.Instances.Start(project, zone, name).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}

	return nil
}

func (c *gceClient) Stop(ctx context.Context, project, zone, name string) error {
	_, err := c.service.Instances.Stop(project, zone, name).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("stopping %s: %w", name, err)
	}

	return nil
}

func (c *gceClient) Delete(ctx context.Context, project, zone, name string) error {
	_, err := c.service.Instances.Delete(project, zone, name).Context(ctx).Do()
	if err != nil {
		// Already gone is the outcome delete wanted: a preempted spot
		// instance whose template says DELETE removes itself, and the lease's
		// release must not report that as a stranded machine.
		if isGoogleAPIStatus(err, 404) {
			return nil
		}

		return fmt.Errorf("deleting %s: %w", name, err)
	}

	return nil
}

func (c *gceClient) Status(ctx context.Context, project, zone, name string) (string, error) {
	instance, err := c.service.Instances.Get(project, zone, name).Context(ctx).Do()
	if err != nil {
		if isGoogleAPIStatus(err, 404) {
			return "", fmt.Errorf("%w: %s", errGCENotFound, name)
		}

		return "", fmt.Errorf("asking about %s: %w", name, err)
	}

	return instance.Status, nil
}

// AddSSHKey is a read-modify-write on the instance's metadata, retried
// because the fingerprint is an optimistic lock: two dials racing — parallel
// jobs sharing a static worker — and the loser's write is refused rather
// than clobbering the winner's key.
func (c *gceClient) AddSSHKey(ctx context.Context, project, zone, name, entry string) error {
	const attempts = 3

	var lastErr error

	for range attempts {
		instance, err := c.service.Instances.Get(project, zone, name).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("reading metadata on %s: %w", name, err)
		}

		metadata := instance.Metadata
		if metadata == nil {
			metadata = &compute.Metadata{}
		}

		merged := mergeSSHKey(metadata, entry)
		if !merged {
			// Already present: another dial from this process got here first.
			return nil
		}

		op, err := c.service.Instances.SetMetadata(project, zone, name, metadata).Context(ctx).Do()
		if err != nil {
			if isGoogleAPIStatus(err, 412) {
				lastErr = err

				continue
			}

			return fmt.Errorf("writing metadata on %s: %w", name, err)
		}

		err = c.awaitZoneOperation(ctx, project, zone, op.Name)
		if err != nil {
			return fmt.Errorf("writing metadata on %s: %w", name, err)
		}

		return nil
	}

	return fmt.Errorf("writing metadata on %s: lost the update race %d times: %w", name, attempts, lastErr)
}

// mergeSSHKey appends one entry to the metadata's ssh-keys item, reporting
// whether anything changed.
func mergeSSHKey(metadata *compute.Metadata, entry string) bool {
	for _, item := range metadata.Items {
		if item.Key != "ssh-keys" {
			continue
		}

		existing := ""
		if item.Value != nil {
			existing = *item.Value
		}

		for line := range strings.SplitSeq(existing, "\n") {
			if strings.TrimSpace(line) == entry {
				return false
			}
		}

		combined := strings.TrimRight(existing, "\n")
		if combined != "" {
			combined += "\n"
		}

		combined += entry
		item.Value = &combined

		return true
	}

	value := entry
	metadata.Items = append(metadata.Items, &compute.MetadataItems{Key: "ssh-keys", Value: &value})

	return true
}

func (c *gceClient) GuestAttributes(ctx context.Context, project, zone, name, path string) (map[string]string, error) {
	attributes, err := c.service.Instances.GetGuestAttributes(project, zone, name).
		QueryPath(path).Context(ctx).Do()
	if err != nil {
		switch {
		case isGoogleAPIStatus(err, 403):
			return nil, fmt.Errorf("%w: %s", errGuestAttributesDisabled, name)
		case isGoogleAPIStatus(err, 404):
			// The namespace does not exist yet: the agent has not published.
			return map[string]string{}, nil
		}

		return nil, fmt.Errorf("reading guest attributes on %s: %w", name, err)
	}

	values := map[string]string{}

	if attributes.QueryValue != nil {
		for _, item := range attributes.QueryValue.Items {
			values[item.Key] = item.Value
		}
	}

	return values, nil
}

// isGoogleAPIStatus reports whether an error is the API answering with one
// HTTP status — the contract, where the message is phrased for a human.
func isGoogleAPIStatus(err error, status int) bool {
	var apiErr *googleapi.Error

	return errors.As(err, &apiErr) && apiErr.Code == status
}

// acquireGCE brings a gcp:// worker's machine into existence, mirroring
// acquire for the aws:// rungs.
func acquireGCE(ctx context.Context, worker Worker) (Worker, func(context.Context, bool) error, error) {
	if !worker.needsAcquisition() {
		return worker, nil, nil
	}

	api, err := gceFor(ctx, worker)
	if err != nil {
		return Worker{}, nil, err
	}

	project, zone, err := gcpLocation(ctx, worker)
	if err != nil {
		return Worker{}, nil, err
	}

	switch worker.Rung {
	case RungStopped:
		return gceStartParked(ctx, api, worker, project, zone)
	case RungLaunch:
		return gceLaunch(ctx, api, worker, project, zone)
	case RungStatic:
		return worker, nil, nil
	default:
		return Worker{}, nil, fmt.Errorf("%w %q: unknown acquisition rung %q", ErrWorker, worker.URL, worker.Rung)
	}
}

// gceStartParked starts a stopped instance and parks it again when the job
// is done with it. A stopped GCE instance reports TERMINATED — Compute
// Engine's word for "stopped, disk kept", not for "gone".
func gceStartParked(ctx context.Context, api gceAPI, worker Worker, project, zone string) (Worker, func(context.Context, bool) error, error) {
	err := api.Start(ctx, project, zone, worker.Instance)
	if err != nil {
		return Worker{}, nil, fmt.Errorf("starting %s for %q: %w", worker.Instance, worker.URL, err)
	}

	err = gceWaitForRunning(ctx, api, worker, project, zone, worker.Instance, "TERMINATED")
	if err != nil {
		// A machine this end started and cannot use is still a machine
		// somebody is paying for, and nothing later will stop it.
		//nolint:contextcheck // deliberately not the caller's context: its being cancelled is the likeliest reason to be here
		gceStopInstance(api, worker, project, zone, worker.Instance)

		return Worker{}, nil, err
	}

	release := func(ctx context.Context, immediate bool) error {
		// The idle window first, exactly as the EC2 parked rung holds one.
		if worker.Idle > 0 && !immediate {
			fmt.Printf("worker %s: holding %s for %s before parking (?idle=0 parks immediately)\n",
				worker.URL, worker.Instance, worker.Idle)

			select {
			case <-time.After(worker.Idle):
			case <-ctx.Done():
			}
		}

		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()

		stopErr := api.Stop(stopCtx, project, zone, worker.Instance)
		if stopErr != nil {
			return fmt.Errorf("stopping %s for %q: %w", worker.Instance, worker.URL, stopErr)
		}

		fmt.Printf("worker %s: parked %s\n", worker.URL, worker.Instance)

		return nil
	}

	fmt.Printf("worker %s: started %s\n", worker.URL, worker.Instance)

	return worker.asStatic(worker.Instance), release, nil
}

// gceStopInstance is the best-effort stop for a machine a failed acquisition
// leaves running, under its own context for the reason cleanupTimeout gives.
func gceStopInstance(api gceAPI, worker Worker, project, zone, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	err := api.Stop(ctx, project, zone, name)
	if err != nil {
		fmt.Printf("warning: could not stop %s after a failed acquisition for %q: %v\n", name, worker.URL, err)
	}
}

// gceLaunch creates one instance from an instance template and deletes it
// when the job ends.
func gceLaunch(ctx context.Context, api gceAPI, worker Worker, project, zone string) (Worker, func(context.Context, bool) error, error) {
	name := gceWorkerName()

	err := api.InsertFromTemplate(ctx, project, zone, name, worker.Template)
	if err != nil {
		return Worker{}, nil, fmt.Errorf("launching a worker for %q: %w", worker.URL, err)
	}

	err = gceWaitForRunning(ctx, api, worker, project, zone, name, "")
	if err != nil {
		// Deleted rather than left behind: a machine this end cannot use is
		// still a machine somebody is paying for.
		//nolint:contextcheck // as gceStopInstance: the caller's context may be the problem
		gceDeleteInstance(api, worker, project, zone, name)

		return Worker{}, nil, err
	}

	release := func(ctx context.Context, _ bool) error {
		deleteCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()

		deleteErr := api.Delete(deleteCtx, project, zone, name)
		if deleteErr != nil {
			return fmt.Errorf("deleting %s for %q: %w", name, worker.URL, deleteErr)
		}

		fmt.Printf("worker %s: deleted %s\n", worker.URL, name)

		return nil
	}

	fmt.Printf("worker %s: launched %s\n", worker.URL, name)

	return worker.asStatic(name), release, nil
}

// gceDeleteInstance is gceStopInstance for a machine that should not exist
// at all once this end cannot use it.
func gceDeleteInstance(api gceAPI, worker Worker, project, zone, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	err := api.Delete(ctx, project, zone, name)
	if err != nil {
		fmt.Printf("warning: could not delete %s after a failed acquisition for %q: %v\n", name, worker.URL, err)
	}
}

// gceWorkerName names a launched instance. Compute Engine requires the
// caller to choose, unlike EC2 — random so two concurrent jobs from one
// template cannot collide.
func gceWorkerName() string {
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)

	return "steps-" + hex.EncodeToString(suffix)
}

// gceWaitForRunning blocks until an instance is RUNNING, or says why it
// never will be — waitForRunning, in Compute Engine's vocabulary.
//
// leaving names the status the instance is departing: TERMINATED for a
// parked machine being started, since the API answers from a replica that
// can report the old status after Start already succeeded. Empty for a
// machine that did not exist a moment ago — there the tolerated lag is
// errGCENotFound, an instance the API briefly denies having created.
func gceWaitForRunning(ctx context.Context, api gceAPI, worker Worker, project, zone, name, leaving string) error {
	deadline, cancel := context.WithTimeout(ctx, acquireTimeout)
	defer cancel()

	ticker := time.NewTicker(acquirePoll)
	defer ticker.Stop()

	left := leaving == ""

	for {
		status, err := api.Status(deadline, project, zone, name)
		if err != nil {
			err = gceWaitOutInvisible(deadline, ticker, worker, name, err)
			if err != nil {
				return err
			}

			continue
		}

		if !left {
			if status == leaving {
				err = awaitTick(deadline, ticker, worker, name)
				if err != nil {
					return err
				}

				continue
			}

			left = true
		}

		arrived, err := gceArrivedOrDead(status, worker, name)
		if arrived || err != nil {
			return err
		}

		err = awaitTick(deadline, ticker, worker, name)
		if err != nil {
			return err
		}
	}
}

// gceWaitOutInvisible sleeps through the API's replica lag, or gives back
// the error that was not lag: every name that reaches the wait was accepted
// by the API itself, so "does not exist" before the first sighting can only
// mean "not yet".
func gceWaitOutInvisible(ctx context.Context, ticker *time.Ticker, worker Worker, name string, cause error) error {
	if !errors.Is(cause, errGCENotFound) {
		return fmt.Errorf("waiting for %s for %q: %w", name, worker.URL, cause)
	}

	return awaitTick(ctx, ticker, worker, name)
}

// gceArrivedOrDead reads a polled status as one of the three answers the
// wait has: it is here, it is never coming, or keep waiting.
func gceArrivedOrDead(status string, worker Worker, name string) (bool, error) {
	switch status {
	case "RUNNING":
		return true, nil
	case "STOPPING", "SUSPENDING", "SUSPENDED", "TERMINATED":
		// A machine going the other way is never going to arrive: a
		// preemption, or an operator's stop, mid-acquisition.
		return false, fmt.Errorf("%w %q: %s went to %s while being acquired", ErrWorker, worker.URL, name, status)
	}

	// PROVISIONING, STAGING, REPAIRING: still on its way.
	return false, nil
}
