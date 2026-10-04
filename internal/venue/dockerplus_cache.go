package venue

// A docker+ worker keeps what it has seen as content-addressed volumes: a data volume holding the tree, and an alias volume named by its digest that points at it. The alias is created only once the data is whole, so a half-filled tree is never found by its digest.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/treedigest"
	"github.com/jtarchie/steps/internal/wire"
)

const (
	cacheLabel  = "steps.cache"
	cacheData   = "steps.data"
	cacheDigest = "steps.digest"
	cacheSize   = "steps.size"
	lowerLabel  = "steps.lower"
	upperLabel  = "steps.upper"
	ovlLabel    = "steps.ovl"
	aliasPrefix = "steps-a-"
	holderMount = "/d"
)

var (
	errNotHeld         = errors.New("the worker no longer holds it")
	errDigestMismatch  = errors.New("the tree that arrived is not the one asked for")
	errStoreOnDocker   = errors.New("a docker+ worker cannot push to the artifact store yet")
	errDigestRunFailed = errors.New("the worker could not digest the tree")
)

// cached finds the data volume holding digest, re-hashing it on the worker first: a volume someone edited, or an alias left pointing at a removed one, is a miss rather than the wrong tree.
func (s *plusSession) cached(ctx context.Context, digest string) (dockerapi.Volume, bool, error) {
	alias, data, err := s.lookup(ctx, digest)
	if err == nil {
		var actual string

		actual, err = s.digestOf(ctx, data.Name)
		if err == nil && actual == digest {
			return data, true, nil
		}
	}

	// A tree the digest script cannot read is as untrustworthy as one that hashes wrong; erroring instead would fail every placement of this digest until someone removed the alias by hand.
	if err != nil && !dockerapi.IsNotFound(err) && !errors.Is(err, errDigestRunFailed) {
		return dockerapi.Volume{}, false, err
	}

	// Stale: forget it, so the next placement fills a fresh one rather than re-checking this.
	if alias.Name != "" {
		_ = s.docker.RemoveVolume(ctx, alias.Name)
	}

	return dockerapi.Volume{}, false, nil
}

// lookup follows digest's alias to its data volume without re-hashing it; alias is zero when there is none.
func (s *plusSession) lookup(ctx context.Context, digest string) (dockerapi.Volume, dockerapi.Volume, error) {
	alias, err := s.docker.InspectVolume(ctx, aliasPrefix+digest)
	if err != nil {
		return dockerapi.Volume{}, dockerapi.Volume{}, fmt.Errorf("%w", err)
	}

	data, err := s.docker.InspectVolume(ctx, alias.Labels[cacheData])
	if err != nil {
		return alias, dockerapi.Volume{}, fmt.Errorf("%w", err)
	}

	return alias, data, nil
}

// fill pours a tree into a new data volume and publishes it under digest; if another session published first, its volume wins and this one is dropped.
func (s *plusSession) fill(ctx context.Context, digest string, pack func(io.Writer) error) (dockerapi.Volume, error) {
	labels := shell.OwnershipLabels()
	labels[cacheLabel] = "data"
	labels[cacheDigest] = digest

	data, err := s.docker.CreateVolume(ctx, "steps-d-"+randomSuffix(), labels, nil)
	if err != nil {
		return dockerapi.Volume{}, fmt.Errorf("%w", err)
	}

	size, err := s.pour(ctx, data.Name, pack)
	if err != nil {
		_ = s.docker.RemoveVolume(context.WithoutCancel(ctx), data.Name)

		return dockerapi.Volume{}, err
	}

	return s.publish(ctx, digest, data, size)
}

// pour reports the bytes it sent, which is what the entry is weighed at for eviction.
func (s *plusSession) pour(ctx context.Context, volume string, pack func(io.Writer) error) (int64, error) {
	holder, err := s.docker.CreateHolder(ctx, "steps-fill-"+randomSuffix(), treedigest.Image, shell.OwnershipLabels(), []string{volumeMount(volume, holderMount, false)})
	if err != nil {
		return 0, fmt.Errorf("%w", err)
	}
	defer func() { _ = s.docker.RemoveContainer(context.WithoutCancel(ctx), holder) }()

	reader, writer := io.Pipe()

	go func() { writer.CloseWithError(pack(writer)) }()

	var poured atomic.Int64

	err = s.docker.PutArchive(ctx, holder, holderMount, &byteCounter{r: reader, n: &poured})

	_ = reader.CloseWithError(io.ErrClosedPipe)

	s.sent.Add(poured.Load())

	if err != nil {
		return 0, fmt.Errorf("sending a tree: %w", err)
	}

	return poured.Load(), nil
}

// publish names data by digest. CreateVolume answers an existing name with that volume, so a lost race shows up as an alias pointing elsewhere.
func (s *plusSession) publish(ctx context.Context, digest string, data dockerapi.Volume, size int64) (dockerapi.Volume, error) {
	labels := shell.OwnershipLabels()
	labels[cacheLabel] = "alias"
	labels[cacheData] = data.Name
	labels[cacheDigest] = digest
	labels[cacheSize] = strconv.FormatInt(size, 10)

	alias, err := s.docker.CreateVolume(ctx, aliasPrefix+digest, labels, nil)
	if err != nil {
		return dockerapi.Volume{}, fmt.Errorf("%w", err)
	}

	if winner := alias.Labels[cacheData]; winner != data.Name {
		_ = s.docker.RemoveVolume(ctx, data.Name)

		winning, err := s.docker.InspectVolume(ctx, winner)
		if err != nil {
			return dockerapi.Volume{}, fmt.Errorf("%w", err)
		}

		return winning, nil
	}

	return data, nil
}

// digestOf is measure without the weighing: a cache hit's re-hash need not walk the tree a second time for a size nobody reads.
func (s *plusSession) digestOf(ctx context.Context, volume string) (string, error) {
	digest, _, err := s.runDigest(ctx, volume, "")

	return digest, err
}

// measure runs treedigest's script against a volume on the worker, read-only and with no network, and weighs it in the same run.
func (s *plusSession) measure(ctx context.Context, volume string) (string, int64, error) {
	return s.runDigest(ctx, volume, ` && du -sk `+holderMount)
}

func (s *plusSession) runDigest(ctx context.Context, volume, weigh string) (string, int64, error) {
	code, stdout, stderr, err := s.docker.RunOnce(ctx, dockerapi.ContainerSpec{
		Image:   treedigest.Image,
		Cmd:     []string{"sh", "-c", `sh -c "$1" sh ` + holderMount + weigh, "sh", treedigest.Script},
		Name:    "steps-digest-" + randomSuffix(),
		Labels:  shell.OwnershipLabels(),
		Network: "none",
		Mounts:  []string{volumeMount(volume, holderMount, true)},
	})
	if err != nil {
		return "", 0, fmt.Errorf("%w", err)
	}

	digest, weight, _ := strings.Cut(stdout, "\n")
	fields := strings.Fields(weight)

	if code != 0 || (weigh != "" && len(fields) == 0) {
		return "", 0, fmt.Errorf("%w (exit %d): %s", errDigestRunFailed, code, stderr)
	}

	if weigh == "" {
		return digest, 0, nil
	}

	kib, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("%w: du said %q", errDigestRunFailed, weight)
	}

	return digest, kib * 1024, nil
}

// overlay gives the step a copy-on-write view of data, so its writes land in a volume of its own and the cached tree stays as it was.
func (s *plusSession) overlay(ctx context.Context, data dockerapi.Volume) (string, error) {
	upper, err := s.newVolume(ctx, "upper")
	if err != nil {
		return "", err
	}

	work, err := s.newVolume(ctx, "ovl")
	if err != nil {
		return "", err
	}

	// Named on the child because docker tracks no dependency between volumes: eviction reads these to leave a lower, and the upper and work dirs no container mounts, alone while the overlay exists.
	labels := shell.OwnershipLabels()
	labels[lowerLabel] = data.Name
	labels[upperLabel] = upper.Name
	labels[ovlLabel] = work.Name

	child, err := s.docker.CreateVolume(ctx, "steps-in-"+randomSuffix(), labels,
		dockerapi.OverlayOptions(data.Mountpoint, upper.Mountpoint, work.Mountpoint))
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	s.volumes = append(s.volumes, child.Name)

	return child.Name, nil
}

// placeLocal is an input the orchestrator has on disk.
func (s *plusSession) placeLocal(ctx context.Context, dir string) (string, error) {
	digest, err := treedigest.Tree(dir)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	data, hit, err := s.cached(ctx, digest)
	if err != nil {
		return "", err
	}

	if !hit {
		data, err = s.fill(ctx, digest, func(w io.Writer) error { return wire.PackTree(w, dir) })
		if err != nil {
			return "", err
		}
	}

	return s.overlay(ctx, data)
}

// placeRemote is an input another step left on a worker: found here by digest, or brought through this machine from its holder.
func (s *plusSession) placeRemote(ctx context.Context, name string, input shell.RemoteInput) (string, error) {
	data, hit, err := s.cached(ctx, input.Digest)
	if err != nil {
		return "", err
	}

	if hit {
		return s.overlay(ctx, data)
	}

	staged, err := os.MkdirTemp("", "steps-pipe-")
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}
	defer func() { _ = os.RemoveAll(staged) }()

	pulled, err := Pull(ctx, shell.RunnerSpec{Worker: input.Holder, ArtifactStore: s.spec.ArtifactStore}, name, input.Digest, staged)
	if err != nil {
		return "", fmt.Errorf("input %q from %s: %w", name, input.Holder, err)
	}

	// Counted as sent even when the worker turns out to hold it: the tree crossed to reach it, which is what a pipe costs.
	s.sent.Add(pulled)

	return s.placeLocal(ctx, staged)
}

// hold files an output volume under its digest and keeps it past close, so the next step here finds it and nothing comes home.
func (s *plusSession) hold(ctx context.Context, volume, digest string, size int64) error {
	// Through cached, which drops an alias whose volume changed since it was filed; publish alone would hand back that volume and drop this one, the real tree.
	existing, hit, err := s.cached(ctx, digest)
	if err != nil {
		return err
	}

	if hit {
		if existing.Name == volume {
			s.kept[volume] = true
		}

		return nil
	}

	data, err := s.docker.InspectVolume(ctx, volume)
	if err != nil {
		return fmt.Errorf("%w", err)
	}

	kept, err := s.publish(ctx, digest, data, size)
	if err != nil {
		return err
	}

	// An identical tree already held elsewhere wins the name; this volume then goes with the session.
	if kept.Name == volume {
		s.kept[volume] = true
	}

	return nil
}

// pullPlus brings a held tree home from a docker+ worker and checks it against the digest it was asked for.
func pullPlus(ctx context.Context, worker Worker, digest, dst string) (int64, error) {
	s := newPlusSession(worker, shell.RunnerSpec{})
	//nolint:contextcheck // close runs under its own bound
	defer func() { _ = s.close() }()

	_, err := s.dialDaemon(ctx)
	if err != nil {
		return 0, err
	}

	// No worker-side re-hash: the digest of what lands in dst below is the check, and hashing first would read the whole tree on the worker one extra time.
	_, data, err := s.lookup(ctx, digest)
	if dockerapi.IsNotFound(err) {
		return 0, fmt.Errorf("%s: %w", digest, errNotHeld)
	}

	if err != nil {
		return 0, err
	}

	holder, err := s.docker.CreateHolder(ctx, "steps-pull-"+randomSuffix(), treedigest.Image, shell.OwnershipLabels(), []string{volumeMount(data.Name, holderMount, true)})
	if err != nil {
		return 0, fmt.Errorf("%w", err)
	}
	defer func() { _ = s.docker.RemoveContainer(context.WithoutCancel(ctx), holder) }()

	content, err := s.docker.GetArchive(ctx, holder, holderMount+"/.")
	if err != nil {
		return 0, fmt.Errorf("%w", err)
	}
	defer func() { _ = content.Close() }()

	err = wire.UnpackFetchedTree(&byteCounter{r: content, n: &s.received}, dst)
	if err != nil {
		return 0, fmt.Errorf("%w", err)
	}

	got, err := treedigest.Tree(dst)
	if err != nil {
		return 0, fmt.Errorf("%w", err)
	}

	if got != digest {
		return 0, fmt.Errorf("%s: %w (got %s)", digest, errDigestMismatch, got)
	}

	return s.received.Load(), nil
}
