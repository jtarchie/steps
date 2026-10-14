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
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jtarchie/steps/internal/events"
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
	InsertFromTemplate(ctx context.Context, project, zone, name, template string, labels map[string]string) error
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
	// Owner reads the instance's steps-owner label and its last start.
	Owner(ctx context.Context, project, zone, name string) (ownedInstance, error)
	// SetOwner writes the steps-owner label, or removes it when value is empty, returning once the write has landed.
	SetOwner(ctx context.Context, project, zone, name, value string) error
	// ListOwned is every instance in the zone on its way up or running and carrying a steps-owner label.
	ListOwned(ctx context.Context, project, zone string) ([]ownedInstance, error)
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
		return newGCEFuncs(&gceClient{service: gceService}), nil
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

	return newGCEFuncs(&gceClient{service: service}), nil
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

func (c *gceClient) InsertFromTemplate(ctx context.Context, project, zone, name, template string, labels map[string]string) error {
	templateURL := "projects/" + project + "/global/instanceTemplates/" + template

	// Labels on the request REPLACE the template's rather than merging, so the template's are read and kept under steps' own: an operator's cost or cleanup label would otherwise vanish from every launched worker.
	merged := map[string]string{}

	// Best effort: inserting from a template needs only instanceTemplates.useReadOnly, and a role without .get must still launch, with steps' labels alone.
	source, err := c.service.InstanceTemplates.Get(project, template).Context(ctx).Do()
	if err == nil && source.Properties != nil {
		maps.Copy(merged, source.Properties.Labels)
	}

	maps.Copy(merged, labels)

	op, err := c.service.Instances.Insert(project, zone, &compute.Instance{Name: name, Labels: merged}).
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

func (c *gceClient) Owner(ctx context.Context, project, zone, name string) (ownedInstance, error) {
	instance, err := c.service.Instances.Get(project, zone, name).Context(ctx).Do()
	if err != nil {
		return ownedInstance{}, fmt.Errorf("asking about %s: %w", name, err)
	}

	return gceOwned(instance), nil
}

// gceOwned falls back to creationTimestamp for an instance that reports no last start, which only one never stopped since its create can be.
func gceOwned(instance *compute.Instance) ownedInstance {
	// Unparseable is the zero time, which no stamp owns.
	started, _ := time.Parse(time.RFC3339, cmp.Or(instance.LastStartTimestamp, instance.CreationTimestamp))

	return ownedInstance{name: instance.Name, stamp: instance.Labels[labelOwner], started: started}
}

// SetOwner is a read-modify-write retried on a lost fingerprint race, as AddSSHKey is, and waits its operation out so a label set before a Start has landed before the Start.
func (c *gceClient) SetOwner(ctx context.Context, project, zone, name, value string) error {
	const attempts = 3

	var lastErr error

	for range attempts {
		instance, err := c.service.Instances.Get(project, zone, name).Context(ctx).Do()
		if err != nil {
			return fmt.Errorf("reading labels on %s: %w", name, err)
		}

		labels := maps.Clone(instance.Labels)
		if labels == nil {
			labels = map[string]string{}
		}

		if value == "" {
			if _, ok := labels[labelOwner]; !ok {
				return nil
			}

			delete(labels, labelOwner)
		} else {
			labels[labelOwner] = value
		}

		op, err := c.service.Instances.SetLabels(project, zone, name, &compute.InstancesSetLabelsRequest{
			Labels: labels, LabelFingerprint: instance.LabelFingerprint,
		}).Context(ctx).Do()
		if err != nil {
			if isGoogleAPIStatus(err, http.StatusPreconditionFailed) {
				lastErr = err

				continue
			}

			return fmt.Errorf("writing labels on %s: %w", name, err)
		}

		err = c.awaitZoneOperation(ctx, project, zone, op.Name)
		if err != nil {
			return fmt.Errorf("writing labels on %s: %w", name, err)
		}

		return nil
	}

	return fmt.Errorf("writing labels on %s: lost the update race %d times: %w", name, attempts, lastErr)
}

// sweepable includes the states a machine is booting through, because a daemon restarted seconds after a create finds its machine there, and no later sweep would look again.
var sweepable = map[string]bool{"PROVISIONING": true, "STAGING": true, "RUNNING": true} //nolint:gochecknoglobals // a constant set

// ListOwned filters client-side: the list filter's grammar for OR and for a hyphenated label key is the part no test here can check.
func (c *gceClient) ListOwned(ctx context.Context, project, zone string) ([]ownedInstance, error) {
	var owned []ownedInstance

	err := c.service.Instances.List(project, zone).Pages(ctx, func(page *compute.InstanceList) error {
		for _, instance := range page.Items {
			if _, ok := instance.Labels[labelOwner]; ok && sweepable[instance.Status] {
				owned = append(owned, gceOwned(instance))
			}
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing instances in %s/%s: %w", project, zone, err)
	}

	return owned, nil
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
func acquireGCE(ctx context.Context, worker Worker) (Worker, func(context.Context) error, error) {
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
func gceStartParked(ctx context.Context, api gceAPI, worker Worker, project, zone string) (Worker, func(context.Context) error, error) {
	// A previous park may still be settling, and starting a STOPPING instance loses a fingerprint race inside GCE (observed).
	status, err := gceAwaitParkComplete(ctx, api, worker, project, zone)
	if err != nil {
		return Worker{}, nil, err
	}

	// Somebody else's, as on EC2: steps stops only what it started — see adoptRunning.
	switch status {
	case "RUNNING", "PROVISIONING", "STAGING":
		err = gceWaitForRunning(ctx, api, worker, project, zone, worker.Instance, "")
		if err != nil {
			return Worker{}, nil, err
		}

		if gceOwns(ctx, api, worker, project, zone) {
			noteReclaimed(ctx, worker)

			return worker.asStatic(worker.Instance), cleanupRelease(worker, worker.Instance, "stopping", "parked", parkGCE(api, worker, project, zone, worker.Instance, true)), nil
		}

		noteAdopted(ctx, worker)

		return worker.asStatic(worker.Instance), nil, nil
	}

	// Labelled before the start, for the reason startParked tags first.
	park := parkGCE(api, worker, project, zone, worker.Instance, labelGCE(ctx, api, worker, project, zone))

	err = api.Start(ctx, project, zone, worker.Instance)
	if err != nil {
		// Start is two-phased like the insert is — the call is ACCEPTED before its operation is waited out — so an error here can be a machine that is already booting, which nothing later would stop.
		//nolint:contextcheck // deliberately not the caller's context: its being cancelled is the likeliest reason to be here
		cleanupAfterFailure(worker, worker.Instance, "venue.stop_failed", park)

		return Worker{}, nil, fmt.Errorf("starting %s for %q: %w", worker.Instance, worker.URL, err)
	}

	err = gceWaitForRunning(ctx, api, worker, project, zone, worker.Instance, "TERMINATED")
	if err != nil {
		// A machine this end started and cannot use is still a machine somebody is paying for, and nothing later will stop it.
		//nolint:contextcheck // deliberately not the caller's context: its being cancelled is the likeliest reason to be here
		cleanupAfterFailure(worker, worker.Instance, "venue.stop_failed", park)

		return Worker{}, nil, err
	}

	release := cleanupRelease(worker, worker.Instance, "stopping", "parked", park)

	events.Note(ctx, events.NoteInfo, fmt.Sprintf("worker %s: started %s", worker.URL, worker.Instance))

	return worker.asStatic(worker.Instance), release, nil
}

// gceAwaitParkComplete waits out an in-flight stop, so Start acts on a
// machine that has finished parking rather than one still on its way down.
func gceAwaitParkComplete(ctx context.Context, api gceAPI, worker Worker, project, zone string) (string, error) {
	var status string

	err := pollUntil(ctx, acquireTimeout, acquirePoll, func(ctx context.Context) (bool, error) {
		var err error

		status, err = api.Status(ctx, project, zone, worker.Instance)
		if err != nil {
			return false, fmt.Errorf("asking about %s for %q before starting it: %w", worker.Instance, worker.URL, err)
		}

		switch status {
		case "STOPPING", "PENDING_STOP", "DEPROVISIONING", "SUSPENDING":
			return false, nil
		}

		return true, nil
	})
	if err != nil {
		return "", waitingFor(err, worker, worker.Instance)
	}

	return status, nil
}

func stopGCE(api gceAPI, project, zone, name string) func(context.Context) error {
	return func(ctx context.Context) error { return api.Stop(ctx, project, zone, name) }
}

// gceOwns is ec2Owns for Compute Engine.
func gceOwns(ctx context.Context, api gceAPI, worker Worker, project, zone string) bool {
	if ownerOf(ctx) == "" {
		return false
	}

	instance, err := api.Owner(ctx, project, zone, worker.Instance)
	if err != nil {
		return false
	}

	_, ours := ownedBy(ownerOf(ctx), instance.stamp, instance.started)

	return ours
}

// labelGCE is tagEC2 for Compute Engine.
func labelGCE(ctx context.Context, api gceAPI, worker Worker, project, zone string) bool {
	owner := ownerOf(ctx)
	if owner == "" {
		return false
	}

	err := api.SetOwner(ctx, project, zone, worker.Instance, stamp(owner, RungStopped, time.Now()))
	if err != nil {
		ownerTagFailed(ctx, worker, worker.Instance, "labelling", err, isGoogleAPIStatus(err, http.StatusForbidden))

		return false
	}

	return true
}

// parkGCE is parkEC2 for Compute Engine.
func parkGCE(api gceAPI, worker Worker, project, zone, name string, labelled bool) func(context.Context) error {
	stop := stopGCE(api, project, zone, name)
	if !labelled {
		return stop
	}

	return untagThen(func(ctx context.Context) {
		err := api.SetOwner(ctx, project, zone, name, "")
		if err != nil {
			ownerTagFailed(ctx, worker, name, "unlabelling", err, isGoogleAPIStatus(err, http.StatusForbidden))
		}
	}, stop)
}

// sweepGCE is the startup sweep at one worker's zone.
func sweepGCE(ctx context.Context, owner string, worker Worker, workers []Worker) error {
	parked, templates := sweepScope(workers)

	api, err := gceFor(ctx, worker)
	if err != nil {
		return err
	}

	project, zone, err := gcpLocation(ctx, worker)
	if err != nil {
		return err
	}

	return sweep(ctx, owner, sweepTarget{
		worker: worker,
		parked: parked,
		// GCE records no template on the instance a label-writer cannot also rewrite, but it names a launch: gceWorkerName's prefix, which nobody can put on an existing instance.
		launched: func(one ownedInstance) bool { return len(templates) > 0 && strings.HasPrefix(one.name, gceNamePrefix) },
		list:     func(ctx context.Context) ([]ownedInstance, error) { return api.ListOwned(ctx, project, zone) },
		park:     func(name string) func(context.Context) error { return parkGCE(api, worker, project, zone, name, true) },
		remove:   func(name string) func(context.Context) error { return deleteGCE(api, project, zone, name) },
	})
}

// gceLaunch creates one instance from an instance template and deletes it
// when the job ends.
func gceLaunch(ctx context.Context, api gceAPI, worker Worker, project, zone string) (Worker, func(context.Context) error, error) {
	name := gceWorkerName()
	labels := worker.launchLabels()

	if owner := ownerOf(ctx); owner != "" {
		labels[labelOwner] = stamp(owner, RungLaunch, time.Now())
	}

	// Named BEFORE the money is spent: the name is client-chosen, so a crash
	// anywhere past the insert leaves a findable trace rather than an
	// anonymous billing machine.
	events.Note(ctx, events.NoteInfo, fmt.Sprintf("worker %s: creating %s from template %s (%s=%s)",
		worker.URL, name, worker.Template, labelWorker, labels[labelWorker]))

	// Labelled in the insert itself, for the reason fleetRequest tags in the fleet request.
	err := api.InsertFromTemplate(ctx, project, zone, name, worker.Template, labels)
	if err != nil {
		// The insert is two-phased — the create is ACCEPTED before its
		// operation is waited out — so an error here (the caller's context
		// cancelled mid-wait, most likely) can leave a machine being built
		// that nothing later will delete. Deleting a machine that was never
		// created answers 404 and is a no-op.
		//nolint:contextcheck // as cleanupAfterFailure: the caller's context may be the problem
		gceDeleteInstance(api, worker, project, zone, name)

		return Worker{}, nil, fmt.Errorf("launching a worker for %q: %w", worker.URL, err)
	}

	err = gceWaitForRunning(ctx, api, worker, project, zone, name, "")
	if err != nil {
		// Deleted rather than left behind: a machine this end cannot use is
		// still a machine somebody is paying for.
		//nolint:contextcheck // as cleanupAfterFailure: the caller's context may be the problem
		gceDeleteInstance(api, worker, project, zone, name)

		return Worker{}, nil, err
	}

	release := cleanupRelease(worker, name, "deleting", "deleted", deleteGCE(api, project, zone, name))

	events.Note(ctx, events.NoteInfo, fmt.Sprintf("worker %s: launched %s", worker.URL, name))

	return worker.asStatic(name), release, nil
}

// gceDeleteInstance is the best-effort give-back for a machine that should not exist
// at all once this end cannot use it.
func gceDeleteInstance(api gceAPI, worker Worker, project, zone, name string) {
	cleanupAfterFailure(worker, name, "venue.delete_failed", deleteGCE(api, project, zone, name))
}

func deleteGCE(api gceAPI, project, zone, name string) func(context.Context) error {
	return func(ctx context.Context) error { return api.Delete(ctx, project, zone, name) }
}

const gceNamePrefix = "steps-"

// gceWorkerName names a launched instance. Compute Engine requires the
// caller to choose, unlike EC2 — random so two concurrent jobs from one
// template cannot collide.
func gceWorkerName() string {
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)

	return gceNamePrefix + hex.EncodeToString(suffix)
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
	left := leaving == ""

	// seen latches once the instance has answered a Status at all — for a
	// parked machine immediately, since its existence was proven before
	// Start. Unlike EC2, where a dead instance stays describable for an
	// hour, GCE answers a DELETED instance with an immediate and permanent
	// 404 — indistinguishable from creation lag only until the first
	// sighting.
	seen := leaving != ""

	err := pollUntil(ctx, acquireTimeout, acquirePoll, func(ctx context.Context) (bool, error) {
		status, err := api.Status(ctx, project, zone, name)
		if err != nil {
			return false, gceWaitOutInvisible(worker, name, err, seen)
		}

		seen = true

		if !left {
			if status == leaving {
				return false, nil
			}

			left = true
		}

		return gceArrivedOrDead(status, worker, name)
	})

	return waitingFor(err, worker, name)
}

// gceWaitOutInvisible reads the API's replica lag as "keep waiting", and
// gives back the error that was not lag: every name that reaches the wait was accepted
// by the API itself, so "does not exist" BEFORE the first sighting can only
// mean "not yet". After one, it means the opposite — a machine that existed
// and now does not was deleted out from under the wait (a preempted spot
// instance whose template says DELETE, most likely), and waiting longer
// would spend the whole timeout on a machine that is never coming back.
func gceWaitOutInvisible(worker Worker, name string, cause error, seen bool) error {
	if !errors.Is(cause, errGCENotFound) {
		return fmt.Errorf("waiting for %s for %q: %w", name, worker.URL, cause)
	}

	if seen {
		return fmt.Errorf("%w %q: %s vanished while being acquired", ErrWorker, worker.URL, name)
	}

	return nil
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
