package venue

// The do:// venue, launch only: a droplet created for a job and deleted with it, a docker+ worker over plain ssh, trusted through the host key steps mints into its cloud-init user_data; its resolved URL do://<id> names a droplet this process launched, whose address and key live in this process's memory.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/oauth2"

	"github.com/jtarchie/steps/internal/events"
)

// The defaults are pocketci's, stated in the docs: do://launch/ alone must name a machine. docker-20-04 is the slug DigitalOcean still publishes its Docker 1-Click under, though the image behind it is built on Ubuntu 22.04 now.
const (
	doDefaultImage  = "docker-20-04"
	doDefaultRegion = "nyc3"
	doDefaultSize   = "s-1vcpu-1gb"
	// doUser is the account cloud-init installs ssh_authorized_keys for on DigitalOcean's images.
	doUser        = "root"
	doFirewallTag = "steps"
)

// doSlug is what DigitalOcean accepts as an image, region or size slug (an image may also be a numeric id), so a mapping that can never create anything is refused when it is read.
var doSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

//nolint:gochecknoglobals // test seams: the API's address, the transport's dial, and boot waits measured in minutes
var (
	doBaseURL = "https://api.digitalocean.com/"
	doDial    = func(ctx context.Context, address string) (net.Conn, error) {
		return (&net.Dialer{Timeout: dialTimeout}).DialContext(ctx, "tcp", address)
	}
	doReadyTimeout = 4 * time.Minute
	doReadyPoll    = 5 * time.Second
)

// doMachine is what this process knows about a droplet it launched: where it answers, which host key it was given, and whether a dial has ever got through.
type doMachine struct {
	address string
	hostKey ssh.PublicKey
	ready   atomic.Bool
}

// doMachines maps a launched droplet's id to its doMachine, from the moment it has an address until it is deleted.
var doMachines sync.Map //nolint:gochecknoglobals // per-process memory of the droplets this process launched

// applyDO reads do://launch/[<image>]. Anything else is either the URL a launched droplet resolved to, which checkDO admits only for a droplet this process launched, or a refusal.
func applyDO(worker Worker, parsed *url.URL) (Worker, error) {
	if parsed.User != nil {
		return Worker{}, fmt.Errorf("%w %q: do:// logs in as %s with a key it mints itself, so a username cannot be chosen", ErrWorker, worker.URL, doUser)
	}

	if parsed.Host != string(RungLaunch) {
		if parsed.Path != "" {
			return Worker{}, errDOLaunchOnly(worker)
		}

		worker.Instance = parsed.Host

		return worker, nil
	}

	image, rest := splitFirstSegment(parsed.Path)
	if rest != "" {
		return Worker{}, fmt.Errorf("%w %q: do:// takes no path past the image — a step's trees live in the droplet's docker volumes", ErrWorker, worker.URL)
	}

	worker.Rung = RungLaunch
	worker.Template = cmp.Or(image, doDefaultImage)

	return worker, nil
}

// doDefaults fills a launch's region and size only when absent: an empty ?region= is an unset shell variable, refused by checkDO rather than read as the default, as parseTemplateVersion refuses ?version=.
func doDefaults(worker Worker, query url.Values) Worker {
	if worker.Rung != RungLaunch {
		return worker
	}

	if !query.Has("region") {
		worker.Region = doDefaultRegion
	}

	if !query.Has("size") {
		worker.Size = doDefaultSize
	}

	return worker
}

func errDOLaunchOnly(worker Worker) error {
	return fmt.Errorf("%w %q: do:// only launches droplets, as do://launch/[image] — a droplet you already own is docker+ssh://root@<ip>?hostkey=SHA256:…, and a stopped one bills at the full rate, so there is no parked rung",
		ErrWorker, worker.URL)
}

// checkDO refuses a do:// mapping that can never create a droplet, or a static one this process did not launch.
func checkDO(worker Worker) error {
	if worker.Rung != RungLaunch {
		if _, ours := doMachines.Load(worker.Instance); !ours {
			return errDOLaunchOnly(worker)
		}

		if worker.Size != "" {
			return fmt.Errorf("%w %q: size= is the shape of a droplet steps launches, and this worker names one that already exists", ErrWorker, worker.URL)
		}

		return nil
	}

	for _, slug := range []struct{ what, value string }{{"image", worker.Template}, {"region", worker.Region}, {"size", worker.Size}} {
		if !doSlug.MatchString(slug.value) {
			return fmt.Errorf("%w %q: %q cannot name a DigitalOcean %s — slugs are lowercase letters, digits, dots and hyphens", ErrWorker, worker.URL, slug.value, slug.what)
		}
	}

	return nil
}

// doClient is DigitalOcean's v2 API, authenticated: four droplet calls over net/http, which godo spent ~1MB of the binary on.
type doClient struct {
	http *http.Client
	base string
}

// doDroplet is the slice of the API's droplet object steps reads. The tests' fake serves godo's own type, which is what holds these tags to the real wire shape.
type doDroplet struct {
	ID       int      `json:"id"`
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	SizeSlug string   `json:"size_slug"`
	Created  string   `json:"created_at"`
	Tags     []string `json:"tags"`
	Region   *struct {
		Slug string `json:"slug"`
	} `json:"region"`
	Image *struct {
		ID   int    `json:"id"`
		Slug string `json:"slug"`
	} `json:"image"`
	Networks *struct {
		V4 []struct {
			IPAddress string `json:"ip_address"`
			Type      string `json:"type"`
		} `json:"v4"`
	} `json:"networks"`
}

func (d *doDroplet) publicIPv4() string {
	if d.Networks == nil {
		return ""
	}

	for _, network := range d.Networks.V4 {
		if network.Type == "public" {
			return network.IPAddress
		}
	}

	return ""
}

type doCreateRequest struct {
	Name     string        `json:"name"`
	Region   string        `json:"region"`
	Size     string        `json:"size"`
	Image    doCreateImage `json:"image"`
	UserData string        `json:"user_data,omitempty"`
	Tags     []string      `json:"tags"`
}

// doCreateImage is an image by slug or by id; the API takes either bare.
type doCreateImage struct {
	ID   int
	Slug string
}

func (i doCreateImage) MarshalJSON() ([]byte, error) {
	if i.Slug != "" {
		return json.Marshal(i.Slug) //nolint:wrapcheck // a string cannot fail to marshal
	}

	return json.Marshal(i.ID) //nolint:wrapcheck // nor an int
}

// doFor builds the API client from the token doctl and terraform also read. Cheap, so built per call rather than cached.
func doFor(worker Worker) (*doClient, error) {
	token := cmp.Or(os.Getenv("DIGITALOCEAN_ACCESS_TOKEN"), os.Getenv("DIGITALOCEAN_TOKEN"))
	if token == "" {
		return nil, fmt.Errorf("%w %q: no DigitalOcean token — set DIGITALOCEAN_ACCESS_TOKEN (or DIGITALOCEAN_TOKEN) to an API token with droplet create, read and delete, and tag create", ErrWorker, worker.URL)
	}

	// Its own bound, since a daemon's step may have no deadline.
	httpClient := &http.Client{
		Timeout:   dialTimeout,
		Transport: &oauth2.Transport{Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})},
	}

	return &doClient{http: httpClient, base: strings.TrimSuffix(doBaseURL, "/")}, nil
}

var errDONotFound = errors.New("the droplet does not exist")

// doAPIError is a non-2xx answer, with the message DigitalOcean put in its body.
type doAPIError struct {
	method, path string
	status       int
	message      string
}

func (e *doAPIError) Error() string {
	return fmt.Sprintf("%s %s: %d %s", e.method, e.path, e.status, e.message)
}

func doStatus(err error, status int) bool {
	var apiErr *doAPIError

	return errors.As(err, &apiErr) && apiErr.status == status
}

// call sends one request and decodes a 2xx body into out, when out is not nil.
func (c *doClient) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader

	if in != nil {
		encoded, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("encoding %s %s: %w", method, path, err)
		}

		body = bytes.NewReader(encoded)
	}

	request, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return fmt.Errorf("building %s %s: %w", method, path, err)
	}

	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "steps")

	if in != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}

	defer func() { _ = response.Body.Close() }()

	if response.StatusCode/100 != 2 {
		var refusal struct {
			Message string `json:"message"`
		}

		_ = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&refusal)

		return &doAPIError{method: method, path: path, status: response.StatusCode, message: cmp.Or(refusal.Message, http.StatusText(response.StatusCode))}
	}

	if out == nil {
		return nil
	}

	err = json.NewDecoder(response.Body).Decode(out)
	if err != nil {
		return fmt.Errorf("reading %s %s: %w", method, path, err)
	}

	return nil
}

func (c *doClient) create(ctx context.Context, request *doCreateRequest) (*doDroplet, error) {
	var answer struct {
		Droplet *doDroplet `json:"droplet"`
	}

	err := c.call(ctx, http.MethodPost, "/v2/droplets", request, &answer)
	if err != nil {
		return nil, err
	}

	return answer.Droplet, nil
}

func (c *doClient) get(ctx context.Context, id int) (*doDroplet, error) {
	var answer struct {
		Droplet *doDroplet `json:"droplet"`
	}

	err := c.call(ctx, http.MethodGet, "/v2/droplets/"+strconv.Itoa(id), nil, &answer)
	if err != nil {
		if doStatus(err, http.StatusNotFound) {
			return nil, fmt.Errorf("%w: %d", errDONotFound, id)
		}

		return nil, fmt.Errorf("asking about droplet %d: %w", id, err)
	}

	if answer.Droplet == nil {
		return nil, fmt.Errorf("asking about droplet %d: the API answered with no droplet", id)
	}

	return answer.Droplet, nil
}

// A 404 is the outcome delete wanted.
func (c *doClient) remove(id int) func(context.Context) error {
	return func(ctx context.Context) error {
		err := c.call(ctx, http.MethodDelete, "/v2/droplets/"+strconv.Itoa(id), nil, nil)
		if err != nil && !doStatus(err, http.StatusNotFound) {
			return fmt.Errorf("deleting droplet %d: %w", id, err)
		}

		return nil
	}
}

// removeNamed deletes whatever answers to a name only this process chose: a create whose answer was lost may still have made a droplet.
func (c *doClient) removeNamed(name string) func(context.Context) error {
	return func(ctx context.Context) error {
		var failures []error

		err := c.eachDroplet(ctx, url.Values{"name": {name}}, func(droplet doDroplet) {
			failures = append(failures, c.remove(droplet.ID)(ctx))
		})

		return errors.Join(append(failures, err)...)
	}
}

// eachDroplet pages through GET /v2/droplets, filtered by query, until links.pages.next runs out.
func (c *doClient) eachDroplet(ctx context.Context, query url.Values, visit func(doDroplet)) error {
	query.Set("per_page", "200")

	for page := 1; ; page++ {
		query.Set("page", strconv.Itoa(page))

		var answer struct {
			Droplets []doDroplet `json:"droplets"`
			Links    struct {
				Pages struct {
					Next string `json:"next"`
				} `json:"pages"`
			} `json:"links"`
		}

		err := c.call(ctx, http.MethodGet, "/v2/droplets?"+query.Encode(), nil, &answer)
		if err != nil {
			return fmt.Errorf("listing droplets: %w", err)
		}

		for _, droplet := range answer.Droplets {
			visit(droplet)
		}

		if answer.Links.Pages.Next == "" || len(answer.Droplets) == 0 {
			return nil
		}
	}
}

// doCloudConfig is the user_data a droplet boots with, written as JSON, which cloud-init reads as the YAML it is a subset of.
type doCloudConfig struct {
	DeleteKeys  bool     `json:"ssh_deletekeys"`
	GenKeyTypes []string `json:"ssh_genkeytypes"`
	SSHKeys     struct {
		ED25519Private string `json:"ed25519_private"`
		ED25519Public  string `json:"ed25519_public"`
	} `json:"ssh_keys"`
	Authorized   []string `json:"ssh_authorized_keys"`
	DisableRoot  bool     `json:"disable_root"`
	PasswordAuth bool     `json:"ssh_pwauth"`
}

// doUserData mints the droplet's host key and writes it, with this process's login key, into the cloud-config that installs both. Password login is turned off, because DigitalOcean mails a root password for a droplet created with no account key.
func doUserData() (ssh.PublicKey, string, error) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("minting a droplet host key: %w", err)
	}

	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		return nil, "", fmt.Errorf("encoding the droplet host key: %w", err)
	}

	hostKey, err := ssh.NewPublicKey(public)
	if err != nil {
		return nil, "", fmt.Errorf("encoding the droplet host key: %w", err)
	}

	signer, err := sshIdentity()
	if err != nil {
		return nil, "", err
	}

	config := doCloudConfig{DeleteKeys: true, GenKeyTypes: []string{}}
	config.SSHKeys.ED25519Private = string(pem.EncodeToMemory(block))
	config.SSHKeys.ED25519Public = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey)))
	config.Authorized = []string{strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))}

	body, err := json.Marshal(config)
	if err != nil {
		return nil, "", fmt.Errorf("writing the droplet's cloud-config: %w", err)
	}

	return hostKey, "#cloud-config\n" + string(body), nil
}

// doTags are launchLabels as DigitalOcean tags, which are bare names: key and value join on the colon a tag allows.
func doTags(ctx context.Context, worker Worker) []string {
	labels := worker.launchLabels()
	if owner := ownerOf(ctx); owner != "" {
		labels[labelOwner] = stamp(owner, RungLaunch, time.Now())
	}

	// The bare tag is what a Cloud Firewall names: the hashed one cannot be known before the droplet exists.
	tags := make([]string, 0, len(labels)+1)
	tags = append(tags, doFirewallTag)

	for _, key := range slices.Sorted(maps.Keys(labels)) {
		tags = append(tags, key+":"+labels[key])
	}

	return tags
}

// acquireDO creates a droplet, waits for it to be active with a public address, and deletes it at release.
func acquireDO(ctx context.Context, worker Worker) (Worker, func(context.Context) error, error) {
	if !worker.needsAcquisition() {
		return worker, nil, nil
	}

	api, err := doFor(worker)
	if err != nil {
		return Worker{}, nil, err
	}

	hostKey, userData, err := doUserData()
	if err != nil {
		return Worker{}, nil, err
	}

	// The same spelling GCE needs, random so concurrent jobs never collide, and chosen here so a lost create can still be found by it.
	name := gceWorkerName()
	tags := doTags(ctx, worker)

	// Named before the money is spent.
	events.Note(ctx, events.NoteInfo, fmt.Sprintf("worker %s: creating droplet %s (%s, %s in %s)", worker.URL, name, worker.Template, worker.Size, worker.Region))
	slog.InfoContext(ctx, "worker.launching", "worker", worker.URL, "droplet", name, "image", worker.Template, "size", worker.Size, "region", worker.Region)

	droplet, err := api.create(ctx, &doCreateRequest{
		Name: name, Region: worker.Region, Size: worker.Size,
		Image: doImage(worker.Template), UserData: userData, Tags: tags,
	})
	if err != nil || droplet == nil {
		//nolint:contextcheck // as cleanupAfterFailure: the caller's context may be the problem
		cleanupAfterFailure(worker, name, "venue.delete_failed", api.removeNamed(name))

		return Worker{}, nil, fmt.Errorf("creating a droplet for %q: %w", worker.URL, cmp.Or(err, errors.New("the API answered with no droplet")))
	}

	instance := strconv.Itoa(droplet.ID)
	remove := api.remove(droplet.ID)

	address, err := doWaitForAddress(ctx, api, worker, droplet.ID)
	if err != nil {
		//nolint:contextcheck // as cleanupAfterFailure: the caller's context may be the problem
		cleanupAfterFailure(worker, instance, "venue.delete_failed", remove)

		return Worker{}, nil, err
	}

	doMachines.Store(instance, &doMachine{address: net.JoinHostPort(address, "22"), hostKey: hostKey})

	release := cleanupRelease(worker, instance, "deleting", "deleted", func(ctx context.Context) error {
		err := remove(ctx)
		if err == nil {
			doMachines.Delete(instance)
		}

		return err
	})

	events.Note(ctx, events.NoteInfo, fmt.Sprintf("worker %s: launched droplet %s (%s) at %s", worker.URL, name, instance, address))

	return worker.asStatic(instance), release, nil
}

// doImage reads an all-digit image as a snapshot or custom image id, anything else as a slug.
func doImage(image string) doCreateImage {
	id, err := strconv.Atoi(image)
	if err == nil {
		return doCreateImage{ID: id}
	}

	return doCreateImage{Slug: image}
}

// doWaitForAddress waits for active AND a public IPv4: the address is assigned after the create answers, and reading it at once can read nothing.
func doWaitForAddress(ctx context.Context, api *doClient, worker Worker, id int) (string, error) {
	var (
		address string
		seen    bool
	)

	err := pollUntil(ctx, acquireTimeout, acquirePoll, func(ctx context.Context) (bool, error) {
		droplet, err := api.get(ctx, id)
		if err != nil {
			if errors.Is(err, errDONotFound) && !seen {
				return false, nil
			}

			if errors.Is(err, errDONotFound) {
				return false, fmt.Errorf("%w %q: droplet %d vanished while being acquired", ErrWorker, worker.URL, id)
			}

			return false, fmt.Errorf("waiting for droplet %d for %q: %w", id, worker.URL, err)
		}

		seen = true

		switch droplet.Status {
		case "new":
			return false, nil
		case "active":
			address = droplet.publicIPv4()

			return address != "", nil
		default:
			return false, fmt.Errorf("%w %q: droplet %d went to %s while being acquired", ErrWorker, worker.URL, id, droplet.Status)
		}
	})

	return address, waitingFor(err, worker, "droplet "+strconv.Itoa(id))
}

// doReadyScript waits out a droplet whose sshd answers before its docker does, for doReadyTimeout.
func doReadyScript() string {
	tries := strconv.Itoa(max(1, int(doReadyTimeout/(2*time.Second))))

	return `i=0; until docker info >/dev/null 2>&1; do i=$((i + 1)); [ "$i" -ge ` + tries + ` ] && break; sleep 2; done
docker info >/dev/null 2>&1 && echo ready`
}

// doSSHClient reaches a launched droplet's sshd, pinned to the host key its user_data carried. Until the first dial gets through, every failure is waited out inside doReadyTimeout — a booting droplet refuses connections and, until cloud-init swaps it, offers the image's own host key, which is refused every time and never accepted.
func doSSHClient(ctx context.Context, worker Worker) (*ssh.Client, error) {
	value, ok := doMachines.Load(worker.Instance)
	if !ok {
		return nil, errDOLaunchOnly(worker)
	}

	machine := value.(*doMachine) //nolint:forcetypeassert // this map holds one type

	signer, err := sshIdentity()
	if err != nil {
		return nil, err
	}

	config := &ssh.ClientConfig{
		User:              doUser,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback:   fixedHostKey(machine.hostKey),
		HostKeyAlgorithms: hostKeyAlgorithms(machine.hostKey),
		Timeout:           dialTimeout,
	}

	if machine.ready.Load() {
		return doConnect(ctx, worker, machine, config)
	}

	client, err := doAwaitBoot(ctx, worker, machine, config)
	if err != nil {
		return nil, err
	}

	session, err := client.NewSession()
	if err != nil {
		_ = client.Close()

		return nil, fmt.Errorf("worker %q: %w", worker.URL, err)
	}

	// session.Output takes no context: closing the client is what ends a wait the job no longer wants.
	stop := context.AfterFunc(ctx, func() { _ = client.Close() })
	out, _ := session.Output(doReadyScript())
	_ = session.Close()

	if !stop() {
		return nil, fmt.Errorf("waiting for docker on droplet %s for %q: %w", worker.Instance, worker.URL, context.Cause(ctx))
	}

	if !strings.Contains(string(out), "ready") {
		_ = client.Close()

		return nil, fmt.Errorf("%w %q: docker never answered on droplet %s — the image must ship docker, as the default %s does", ErrWorker, worker.URL, worker.Instance, doDefaultImage)
	}

	machine.ready.Store(true)

	return client, nil
}

func doAwaitBoot(ctx context.Context, worker Worker, machine *doMachine, config *ssh.ClientConfig) (*ssh.Client, error) {
	deadline, cancel := context.WithTimeout(ctx, doReadyTimeout)
	defer cancel()

	ticker := time.NewTicker(doReadyPoll)
	defer ticker.Stop()

	// A mismatch outranks whatever the deadline cut off last: past the boot window, it is the alarm.
	var mismatch error

	for {
		client, err := doConnect(deadline, worker, machine, config)
		if err == nil {
			return client, nil
		}

		if errors.Is(err, errHostKeyMismatch) {
			mismatch = err
		}

		select {
		case <-ticker.C:
		case <-deadline.Done():
			// The job ending is not the boot window running out, and a mismatch seen on the way must not be reported as the reason.
			if ctx.Err() != nil {
				return nil, fmt.Errorf("waiting for droplet %s's sshd: %w", worker.Instance, context.Cause(ctx))
			}

			return nil, fmt.Errorf("waiting %s for droplet %s's sshd: %w", doReadyTimeout, worker.Instance, cmp.Or(mismatch, err))
		}
	}
}

func doConnect(ctx context.Context, worker Worker, machine *doMachine, config *ssh.ClientConfig) (*ssh.Client, error) {
	conn, err := doDial(ctx, machine.address)
	if err != nil {
		return nil, fmt.Errorf("worker %q: %w", worker.URL, err)
	}

	client, err := sshHandshake(conn, machine.address, config)
	if err != nil {
		return nil, fmt.Errorf("connecting to droplet %s at %s for %q: %w", worker.Instance, machine.address, worker.URL, err)
	}

	return client, nil
}

// doSweepable are the states a droplet bills in.
var doSweepable = map[string]bool{"new": true, "active": true} //nolint:gochecknoglobals // a constant set

// sweepDO is the startup sweep in one region. A stamp is a tag anyone with tag write can forge, so a droplet is this daemon's only when it also has the image and size a mapping here launches, which DigitalOcean itself records.
func sweepDO(ctx context.Context, owner string, worker Worker, workers []Worker) error {
	api, err := doFor(worker)
	if err != nil {
		return err
	}

	shapes := map[string]bool{}

	for _, one := range workers {
		if one.Rung == RungLaunch {
			shapes[one.Template+"/"+one.Size] = true
		}
	}

	remove := func(name string) func(context.Context) error {
		id, err := strconv.Atoi(name)
		if err != nil {
			return func(context.Context) error { return fmt.Errorf("droplet %q: %w", name, err) }
		}

		return api.remove(id)
	}

	return sweep(ctx, owner, sweepTarget{
		worker: worker,
		parked: map[string]bool{},
		launched: func(one ownedInstance) bool {
			return slices.ContainsFunc(strings.Fields(one.template), func(shape string) bool { return shapes[shape] })
		},
		list: func(ctx context.Context) ([]ownedInstance, error) {
			var owned []ownedInstance

			err := api.eachDroplet(ctx, url.Values{}, func(droplet doDroplet) {
				if one, ok := doOwned(droplet, worker.Region); ok {
					owned = append(owned, one)
				}
			})

			return owned, err
		},
		park:   remove,
		remove: remove,
	})
}

// doOwned reads a droplet's stamp, creation and shape, for one that bills in region and carries a stamp at all. Created is its start: a droplet is never stopped by steps.
func doOwned(droplet doDroplet, region string) (ownedInstance, bool) {
	if !doSweepable[droplet.Status] || droplet.Region == nil || droplet.Region.Slug != region {
		return ownedInstance{}, false
	}

	for _, tag := range droplet.Tags {
		value, found := strings.CutPrefix(tag, labelOwner+":")
		if !found {
			continue
		}

		// Both spellings, space-separated: a mapping may name a slugged image by its id, and DigitalOcean records the slug.
		var shapes []string
		if droplet.Image != nil {
			shapes = append(shapes, strconv.Itoa(droplet.Image.ID)+"/"+droplet.SizeSlug)
			if droplet.Image.Slug != "" {
				shapes = append(shapes, droplet.Image.Slug+"/"+droplet.SizeSlug)
			}
		}

		// Unparseable is the zero time, which no stamp owns.
		created, _ := time.Parse(time.RFC3339, droplet.Created)

		return ownedInstance{name: strconv.Itoa(droplet.ID), stamp: value, started: created, template: strings.Join(shapes, " ")}, true
	}

	return ownedInstance{}, false
}
