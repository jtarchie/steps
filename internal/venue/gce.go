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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
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
	gceServiceMu.Lock()
	defer gceServiceMu.Unlock()

	if gceService != nil {
		return &gceClient{service: gceService}, nil
	}

	// Not the caller's context, for the reason gcpToken gives: the service
	// carries its own token source, and a refresh an hour from now must not
	// fail because the step that happened to build it was cancelled.
	service, err := compute.NewService(context.WithoutCancel(ctx))
	if err != nil {
		return nil, fmt.Errorf("%w %q: no GCP credentials (log in with `gcloud auth application-default login`): %w",
			ErrWorker, worker.URL, err)
	}

	gceService = service

	return &gceClient{service: service}, nil
}

// gceService is the process's Compute Engine client, resolved once and cached
// only on success. A session is dialled per STEP, so without this every
// placed step re-read the credentials, re-minted a token and opened a fresh
// connection pool — the same cost artifactStores caches away for the data
// plane. Nothing about the client is worker-specific, so one serves them all.
//
//nolint:gochecknoglobals // one credential resolution per process, documented above
var (
	gceServiceMu sync.Mutex
	gceService   *compute.Service
)

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
// it says went wrong. Bounded by acquireTimeout on top of the caller's
// context, because every other acquisition wait is and a wedged operation
// would otherwise hold a lease forever in a daemon with no step timeout.
func (c *gceClient) awaitZoneOperation(ctx context.Context, project, zone, operation string) error {
	deadline, cancel := context.WithTimeout(ctx, acquireTimeout)
	defer cancel()

	for {
		op, err := c.service.ZoneOperations.Wait(project, zone, operation).Context(deadline).Do()
		if err != nil {
			return fmt.Errorf("waiting for the operation: %w", err)
		}

		if op.Status == "DONE" {
			return zoneOperationOutcome(op)
		}

		// Wait normally holds the request open up to two minutes, but the
		// SDK documents it as best-effort — under load it "might return
		// after zero seconds" — so the pause is what keeps a hot server from
		// being hammered in a tight loop.
		select {
		case <-deadline.Done():
			return fmt.Errorf("waiting for the operation: %w", deadline.Err())
		case <-time.After(time.Second):
		}
	}
}

// zoneOperationOutcome reads a DONE operation's verdict. The Error list is
// the usual carrier, but the API also expresses failure as a bare HTTP
// status on the operation — belt and braces, since the docs promise neither
// shape exclusively and a failure read as success becomes a silent
// ten-minute wait for a machine that was never coming.
func zoneOperationOutcome(op *compute.Operation) error {
	if op.Error != nil && len(op.Error.Errors) > 0 {
		first := op.Error.Errors[0]

		return fmt.Errorf("the operation failed: %s: %s", first.Code, first.Message)
	}

	if op.HttpErrorStatusCode >= http.StatusBadRequest {
		return fmt.Errorf("the operation failed: HTTP %d: %s", op.HttpErrorStatusCode, op.HttpErrorMessage)
	}

	return nil
}

// Start waits the operation out for the same reason InsertFromTemplate does:
// GCE reports a start that cannot happen — an exhausted zone, a fingerprint
// conflict — in the operation, not the accepting call, and skipping the wait
// turns each into a silent ten-minute poll of an instance that stays parked.
func (c *gceClient) Start(ctx context.Context, project, zone, name string) error {
	op, err := c.service.Instances.Start(project, zone, name).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("starting %s: %w", name, err)
	}

	err = c.awaitZoneOperation(ctx, project, zone, op.Name)
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

// mergeSSHKey merges one entry into the metadata's ssh-keys item, pruning
// expired google-ssh entries as it goes, and reports whether anything
// changed.
//
// The pruning is this end's job, not the guest agent's: the agent only
// stops HONORING an expired key — it has no credentials to write metadata
// back (the worker may hold no service account at all) — so without a
// client-side sweep every install would append forever, until the value hit
// GCE's 256KB metadata cap and no key could be installed at all. gcloud
// prunes on install for exactly this reason.
func mergeSSHKey(metadata *compute.Metadata, entry string) bool {
	for _, item := range metadata.Items {
		if item.Key != "ssh-keys" {
			continue
		}

		existing := ""
		if item.Value != nil {
			existing = *item.Value
		}

		kept, present, pruned := liveSSHKeys(existing, entry)
		if !present {
			kept = append(kept, entry)
		}

		if present && !pruned {
			return false
		}

		combined := strings.Join(kept, "\n")
		item.Value = &combined

		return true
	}

	value := entry
	metadata.Items = append(metadata.Items, &compute.MetadataItems{Key: "ssh-keys", Value: &value})

	return true
}

// liveSSHKeys filters an ssh-keys value down to the lines worth keeping,
// reporting whether entry was among them and whether anything was dropped.
func liveSSHKeys(existing, entry string) (kept []string, present, pruned bool) {
	for line := range strings.SplitSeq(existing, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		if expiredSSHKey(trimmed) {
			pruned = true

			continue
		}

		if trimmed == entry {
			present = true
		}

		kept = append(kept, trimmed)
	}

	return kept, present, pruned
}

// expiredSSHKey reports whether a line is a google-ssh entry whose expireOn
// has passed. Anything unparseable is kept — a permanent key an operator
// installed by hand is not this end's to remove.
func expiredSSHKey(line string) bool {
	_, rest, found := strings.Cut(line, " google-ssh ")
	if !found {
		return false
	}

	var grant struct {
		ExpireOn string `json:"expireOn"`
	}

	if json.Unmarshal([]byte(rest), &grant) != nil || grant.ExpireOn == "" {
		return false
	}

	expiry, err := time.Parse(gcpKeyExpiryLayout, grant.ExpireOn)
	if err != nil {
		return false
	}

	return time.Now().After(expiry)
}

func (c *gceClient) GuestAttributes(ctx context.Context, project, zone, name, path string) (map[string]string, error) {
	attributes, err := c.service.Instances.GetGuestAttributes(project, zone, name).
		QueryPath(path).Context(ctx).Do()
	if err != nil {
		switch {
		case isGoogleAPIStatus(err, 403):
			// 403 is also how the API answers a caller without
			// compute.instances.getGuestAttributes, so the API's own message
			// travels: it is the only thing that tells the two apart, and
			// discarding it sends an operator to edit a template that was
			// already correct.
			return nil, fmt.Errorf("%w: %s: %w", errGuestAttributesDisabled, name, err)
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
	// A previous job's park may still be settling: release returns once the
	// stop is ACCEPTED, and starting again while the instance is STOPPING
	// loses a fingerprint race inside GCE ("the resource fingerprint changed
	// during the start operation" — observed). Back-to-back serial jobs on
	// one parked worker hit this window every time, so wait it out first.
	err := gceAwaitParkComplete(ctx, api, worker, project, zone)
	if err != nil {
		return Worker{}, nil, err
	}

	err = api.Start(ctx, project, zone, worker.Instance)
	if err != nil {
		// Start is two-phased like the insert is — the call is ACCEPTED
		// before its operation is waited out — so an error here can be a
		// machine that is already booting, which nothing later would stop.
		//nolint:contextcheck // deliberately not the caller's context: its being cancelled is the likeliest reason to be here
		gceStopInstance(api, worker, project, zone, worker.Instance)

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
		holdIdleWindow(ctx, worker, immediate)

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

// gceAwaitParkComplete waits out an in-flight stop, so Start acts on a
// machine that has finished parking rather than one still on its way down.
func gceAwaitParkComplete(ctx context.Context, api gceAPI, worker Worker, project, zone string) error {
	deadline, cancel := context.WithTimeout(ctx, acquireTimeout)
	defer cancel()

	ticker := time.NewTicker(acquirePoll)
	defer ticker.Stop()

	for {
		status, err := api.Status(deadline, project, zone, worker.Instance)
		if err != nil {
			return fmt.Errorf("asking about %s for %q before starting it: %w", worker.Instance, worker.URL, err)
		}

		switch status {
		case "STOPPING", "PENDING_STOP", "DEPROVISIONING", "SUSPENDING":
		default:
			return nil
		}

		err = awaitTick(deadline, ticker, worker, worker.Instance)
		if err != nil {
			return err
		}
	}
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

	// Named BEFORE the money is spent: the name is client-chosen, so a crash
	// anywhere past the insert leaves a findable trace rather than an
	// anonymous billing machine.
	fmt.Printf("worker %s: creating %s from template %s\n", worker.URL, name, worker.Template)

	err := api.InsertFromTemplate(ctx, project, zone, name, worker.Template)
	if err != nil {
		// The insert is two-phased — the create is ACCEPTED before its
		// operation is waited out — so an error here (the caller's context
		// cancelled mid-wait, most likely) can leave a machine being built
		// that nothing later will delete. Deleting a machine that was never
		// created answers 404 and is a no-op.
		//nolint:contextcheck // as gceStopInstance: the caller's context may be the problem
		gceDeleteInstance(api, worker, project, zone, name)

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

	// seen latches once the instance has answered a Status at all — for a
	// parked machine immediately, since its existence was proven before
	// Start. Unlike EC2, where a dead instance stays describable for an
	// hour, GCE answers a DELETED instance with an immediate and permanent
	// 404 — indistinguishable from creation lag only until the first
	// sighting.
	seen := leaving != ""

	for {
		status, err := api.Status(deadline, project, zone, name)
		if err != nil {
			err = gceWaitOutInvisible(deadline, ticker, worker, name, err, seen)
			if err != nil {
				return err
			}

			continue
		}

		seen = true

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
// by the API itself, so "does not exist" BEFORE the first sighting can only
// mean "not yet". After one, it means the opposite — a machine that existed
// and now does not was deleted out from under the wait (a preempted spot
// instance whose template says DELETE, most likely), and waiting longer
// would spend the whole timeout on a machine that is never coming back.
func gceWaitOutInvisible(ctx context.Context, ticker *time.Ticker, worker Worker, name string, cause error, seen bool) error {
	if !errors.Is(cause, errGCENotFound) {
		return fmt.Errorf("waiting for %s for %q: %w", name, worker.URL, cause)
	}

	if seen {
		return fmt.Errorf("%w %q: %s vanished while being acquired", ErrWorker, worker.URL, name)
	}

	return awaitTick(ctx, ticker, worker, name)
}

// gceArrivedOrDead reads a polled status as one of the three answers the
// wait has: it is here, it is never coming, or keep waiting.
func gceArrivedOrDead(status string, worker Worker, name string) (bool, error) {
	switch status {
	case "RUNNING":
		return true, nil
	case "STOPPING", "PENDING_STOP", "DEPROVISIONING", "SUSPENDING", "SUSPENDED", "TERMINATED":
		// A machine going the other way is never going to arrive: a
		// preemption, or an operator's stop, mid-acquisition. PENDING_STOP
		// and DEPROVISIONING are the graceful-shutdown and teardown phases
		// of the same journey.
		return false, fmt.Errorf("%w %q: %s went to %s while being acquired", ErrWorker, worker.URL, name, status)
	}

	// PROVISIONING, STAGING, REPAIRING, PENDING: still on its way. STOPPED
	// deliberately waits too — it is a resting state a parked machine can be
	// STARTED from, and the parked rung's wait must be able to watch one
	// depart it.
	return false, nil
}
