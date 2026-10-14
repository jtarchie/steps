package venue

// do:// against a fake DigitalOcean API — godo itself, aimed at httptest — and the repo's test sshd, which boots with whatever host key the create request's user_data handed it, the way cloud-init does.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitalocean/godo"
	"golang.org/x/crypto/ssh"

	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/testsshd"
)

type fakeDroplet struct {
	godo.Droplet

	gets int
	sshd *testSSHD
}

type fakeDO struct {
	t  *testing.T
	mu sync.Mutex

	nextID   int
	droplets map[int]*fakeDroplet
	creates  []godo.DropletCreateRequest
	deletes  []int
	// noIPPolls is how many active answers come back before the droplet has a public address.
	noIPPolls int
	// status, when set, is what every GET reports in place of the boot sequence.
	status string
	// createStatus fails POST /v2/droplets with that HTTP status, after creating the droplet when createThenFail.
	createStatus   int
	createThenFail bool
	// imageKeyDials is how many dials reach an sshd still holding the image's own host key, before cloud-init swaps it.
	imageKeyDials int
	// wrongKeyForever serves an impostor host key on every dial.
	wrongKeyForever bool
	// vanishAfter deletes a droplet out from under the wait after that many GETs found it.
	vanishAfter int
	// noBoot skips the sshd, for the tests that never dial.
	noBoot bool

	dialed []string
}

func newFakeDO(t *testing.T) *fakeDO {
	t.Helper()

	fake := &fakeDO{t: t, nextID: 1000, droplets: map[int]*fakeDroplet{}}
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)

	shrinkAcquireWaits(t)
	shrinkDOWaits(t)

	t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", "test-token")
	t.Setenv("DIGITALOCEAN_TOKEN", "")

	previousURL, previousDial := doBaseURL, doDial
	doBaseURL = server.URL + "/"
	doDial = fake.dial

	t.Cleanup(func() { doBaseURL, doDial = previousURL, previousDial })

	return fake
}

func shrinkDOWaits(t *testing.T) {
	t.Helper()

	previousTimeout, previousPoll := doReadyTimeout, doReadyPoll
	doReadyTimeout, doReadyPoll = 5*time.Second, 20*time.Millisecond

	t.Cleanup(func() { doReadyTimeout, doReadyPoll = previousTimeout, previousPoll })
}

const doNotFound = `{"id":"not_found","message":"The resource you were accessing could not be found."}`

func (f *fakeDO) handler() http.Handler {
	mux := http.NewServeMux()
	locked := func(handle func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()

			handle(w, r)
		}
	}

	mux.HandleFunc("POST /v2/droplets", locked(f.create))
	mux.HandleFunc("GET /v2/droplets", locked(f.list))
	mux.HandleFunc("GET /v2/droplets/{id}", locked(f.get))
	mux.HandleFunc("DELETE /v2/droplets/{id}", locked(f.remove))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, `{"id":"unauthorized","message":"bad token"}`, http.StatusUnauthorized)

			return
		}

		mux.ServeHTTP(w, r)
	})
}

func (f *fakeDO) remove(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	if _, ok := f.droplets[id]; !ok {
		http.Error(w, doNotFound, http.StatusNotFound)

		return
	}

	delete(f.droplets, id)
	f.deletes = append(f.deletes, id)
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeDO) create(w http.ResponseWriter, r *http.Request) {
	// godo writes the image as a bare slug or id, which its own type does not read back.
	var body struct {
		godo.DropletCreateRequest

		Image json.RawMessage `json:"image"`
	}

	err := json.NewDecoder(r.Body).Decode(&body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	request := body.DropletCreateRequest
	if json.Unmarshal(body.Image, &request.Image.Slug) != nil {
		_ = json.Unmarshal(body.Image, &request.Image.ID)
	}

	f.creates = append(f.creates, request)

	if f.createStatus != 0 && !f.createThenFail {
		http.Error(w, `{"id":"unprocessable_entity","message":"refused"}`, f.createStatus)

		return
	}

	f.nextID++
	droplet := &fakeDroplet{Droplet: godo.Droplet{
		ID: f.nextID, Name: request.Name, Status: "new",
		Region: &godo.Region{Slug: request.Region}, Image: &godo.Image{Slug: request.Image.Slug},
		SizeSlug: request.Size, Tags: request.Tags, Created: time.Now().UTC().Format(time.RFC3339),
	}}

	if !f.noBoot {
		droplet.sshd = bootFromUserData(f.t, request.UserData) //nolint:contextcheck // the sshd lives as long as the test, not the request
	}

	f.droplets[droplet.ID] = droplet

	if f.createThenFail {
		http.Error(w, `{"id":"server_error","message":"lost"}`, f.createStatus)

		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"droplet": droplet.Droplet})
}

func (f *fakeDO) get(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))

	droplet, ok := f.droplets[id]
	if !ok {
		http.Error(w, doNotFound, http.StatusNotFound)

		return
	}

	droplet.gets++

	if f.vanishAfter > 0 && droplet.gets > f.vanishAfter {
		delete(f.droplets, id)
		http.Error(w, doNotFound, http.StatusNotFound)

		return
	}

	switch {
	case f.status != "":
		droplet.Status = f.status
	case droplet.gets == 1:
		droplet.Status = "new"
	default:
		droplet.Status = "active"
	}

	if droplet.Status == "active" && droplet.gets > 1+f.noIPPolls {
		droplet.Networks = &godo.Networks{V4: []godo.NetworkV4{
			{IPAddress: "10.10.0.5", Type: "private"},
			{IPAddress: "203.0.113." + strconv.Itoa(id%250), Type: "public"},
		}}
	}

	writeJSON(w, http.StatusOK, map[string]any{"droplet": droplet.Droplet})
}

func (f *fakeDO) list(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")

	var found []godo.Droplet

	for _, droplet := range f.droplets {
		if name == "" || droplet.Name == name {
			found = append(found, droplet.Droplet)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"droplets": found, "links": map[string]any{}, "meta": map[string]any{"total": len(found)}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	err := json.NewEncoder(w).Encode(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// dial is doDial: it records the address steps asked for, and reaches the sshd booted for the droplet whose public IP that is.
func (f *fakeDO) dial(ctx context.Context, address string) (net.Conn, error) {
	f.mu.Lock()

	f.dialed = append(f.dialed, address)

	var target *fakeDroplet

	for id, droplet := range f.droplets {
		if address == "203.0.113."+strconv.Itoa(id%250)+":22" {
			target = droplet
		}
	}

	impostor := f.wrongKeyForever || f.imageKeyDials > 0
	if f.imageKeyDials > 0 {
		f.imageKeyDials--
	}

	f.mu.Unlock()

	if target == nil || target.sshd == nil {
		return nil, fmt.Errorf("dial %s: connection refused", address)
	}

	addr := target.sshd.Addr()
	if impostor {
		addr = impostorSSHD(f.t).Addr() //nolint:contextcheck // the sshd lives as long as the test, not the dial
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("reaching the test sshd: %w", err)
	}

	return conn, nil
}

// bootFromUserData is cloud-init: the host key and the authorized key both come out of user_data, or the droplet never answers.
func bootFromUserData(t *testing.T, userData string) *testSSHD {
	t.Helper()

	config, err := parseCloudConfig(userData)
	if err != nil {
		t.Errorf("user_data is not the cloud-config steps writes: %v", err)

		return nil
	}

	hostSigner, err := ssh.ParsePrivateKey([]byte(config.SSHKeys.ED25519Private))
	if err != nil {
		t.Errorf("user_data's host private key: %v", err)

		return nil
	}

	authorized := map[string]bool{}
	for _, line := range config.Authorized {
		key, _, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(line))
		if parseErr == nil {
			authorized[string(key.Marshal())] = true
		}
	}

	server := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if meta.User() != doUser || !authorized[string(key.Marshal())] {
				return nil, errors.New("unknown key")
			}

			return &ssh.Permissions{}, nil
		},
	}
	server.AddHostKey(hostSigner)

	return testsshd.NewWithConfig(t, server, hostSigner.PublicKey())
}

// impostorSSHD holds some other host key: the image's own before cloud-init swaps it, or a machine in the middle.
func impostorSSHD(t *testing.T) *testSSHD {
	t.Helper()

	signer, pub, _ := generateKey(t)

	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)

	return testsshd.NewWithConfig(t, config, pub)
}

func parseCloudConfig(userData string) (doCloudConfig, error) {
	body, found := strings.CutPrefix(userData, "#cloud-config\n")
	if !found {
		return doCloudConfig{}, errors.New("no #cloud-config header")
	}

	var config doCloudConfig

	err := json.Unmarshal([]byte(body), &config)
	if err != nil {
		return doCloudConfig{}, fmt.Errorf("user_data is not JSON: %w", err)
	}

	return config, nil
}

func (f *fakeDO) snapshot() (creates []godo.DropletCreateRequest, deletes []int, live int) {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]godo.DropletCreateRequest(nil), f.creates...), append([]int(nil), f.deletes...), len(f.droplets)
}

// TestVenueRunsAStepOnADOLaunchedDroplet is the feature: a placed step on do://launch/ creates a droplet whose user_data carries the keys, dials the address the API reported against the host key steps minted, runs in a container there, brings its output home, and deletes the droplet.
func TestVenueRunsAStepOnADOLaunchedDroplet(t *testing.T) {
	seamCloudSocket(t, hostDockerSocket(t))

	fake := newFakeDO(t)
	fake.noIPPolls = 1

	worker, err := ParseWorker("do://launch/?idle=0s")
	if err != nil {
		t.Fatalf("ParseWorker: %v", err)
	}

	registry := NewRegistry("ownerid")
	leases := registry.Leases(map[string]Worker{"do": worker})

	resolved, err := leases.Resolve(context.Background(), "do")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	cwd := t.TempDir()
	mustWrite(t, filepath.Join(cwd, "data", "seed.txt"), "seed\n")
	mustMkdir(t, filepath.Join(cwd, "out"))

	runner := newLocalRunner(t, shell.RunnerSpec{Cwd: cwd, Image: "alpine:3", Worker: resolved.URL, Fetch: []string{"out"}})

	err = shell.Run(context.Background(), runner, "cat data/seed.txt > out/report.txt")
	if err != nil {
		t.Fatalf("Run on a do:// worker: %v", err)
	}

	_ = runner.Close()

	got := mustRead(t, filepath.Join(cwd, "out", "report.txt"))
	creates, _, _ := fake.snapshot()

	if got != "seed\n" || len(creates) != 1 {
		t.Fatalf("out/report.txt = %q after %d creates, want %q after one", got, len(creates), "seed\n")
	}

	assertDOCreate(t, creates[0])
	fake.assertDialedTheReportedAddress(t)

	err = leases.ReleaseAll(context.Background())
	if err != nil {
		t.Fatalf("ReleaseAll: %v", err)
	}

	_, deletes, live := fake.snapshot()
	if len(deletes) != 1 || strconv.Itoa(deletes[0]) != resolved.Instance || live != 0 {
		t.Errorf("deletes = %v with %d left, want droplet %s deleted", deletes, live, resolved.Instance)
	}
}

func (f *fakeDO) assertDialedTheReportedAddress(t *testing.T) {
	t.Helper()

	f.mu.Lock()
	defer f.mu.Unlock()

	want := "203.0.113." + strconv.Itoa(f.nextID%250) + ":22"
	if len(f.dialed) == 0 || f.dialed[0] != want {
		t.Errorf("dialed %v, want the public address the API reported: %s", f.dialed, want)
	}
}

func assertDOCreate(t *testing.T, create godo.DropletCreateRequest) {
	t.Helper()

	if create.Region != "nyc3" || create.Size != "s-1vcpu-1gb" || create.Image.Slug != "docker-20-04" {
		t.Errorf("created %s/%s/%s, want the documented defaults nyc3/s-1vcpu-1gb/docker-20-04", create.Region, create.Size, create.Image.Slug)
	}

	if len(create.SSHKeys) != 0 {
		t.Errorf("ssh_keys = %v, want none: no key is uploaded to the account", create.SSHKeys)
	}

	assertDOCloudConfig(t, create.UserData)
	assertDOTags(t, create.Tags)
}

func assertDOCloudConfig(t *testing.T, userData string) {
	t.Helper()

	config, err := parseCloudConfig(userData)
	if err != nil {
		t.Fatalf("user_data: %v", err)
	}

	signer, err := sshIdentity()
	if err != nil {
		t.Fatalf("sshIdentity: %v", err)
	}

	login := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if len(config.Authorized) != 1 || config.Authorized[0] != login {
		t.Errorf("ssh_authorized_keys = %v, want this process's login key", config.Authorized)
	}

	if !config.DeleteKeys || config.PasswordAuth || config.SSHKeys.ED25519Private == "" || config.SSHKeys.ED25519Public == "" {
		t.Errorf("cloud-config = %+v, want minted host keys, ssh_deletekeys and no password login", config)
	}
}

func assertDOTags(t *testing.T, tags []string) {
	t.Helper()

	if !slices.Contains(tags, "steps") {
		t.Errorf("tags %v carry no bare steps tag for a firewall to name", tags)
	}

	joined := strings.Join(tags, " ")
	for _, want := range []string{labelWorker + ":", labelHost + ":", labelPid + ":", labelOwner + ":ownerid-launch-"} {
		if !strings.Contains(joined, want) {
			t.Errorf("tags %v carry no %s", tags, want)
		}
	}
}
