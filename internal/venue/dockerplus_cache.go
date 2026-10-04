package venue

// A docker+ worker keeps what it has seen as content-addressed volumes: a data volume holding the tree, and an alias volume named by its digest that points at it. The alias is created only once the data is whole, so a half-filled tree is never found by its digest.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jtarchie/steps/internal/dockerapi"
	"github.com/jtarchie/steps/internal/shell"
	"github.com/jtarchie/steps/internal/treedigest"
	"github.com/jtarchie/steps/internal/wire"
)

const (
	cacheLabel  = "steps.cache"
	cacheData   = "steps.data"
	cacheDigest = "steps.digest"
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
	alias, err := s.docker.InspectVolume(ctx, aliasPrefix+digest)
	if dockerapi.IsNotFound(err) {
		return dockerapi.Volume{}, false, nil
	}

	if err != nil {
		return dockerapi.Volume{}, false, fmt.Errorf("%w", err)
	}

	data, err := s.docker.InspectVolume(ctx, alias.Labels[cacheData])
	if err == nil {
		var actual string

		actual, err = s.digestOf(ctx, data.Name)
		if err == nil && actual == digest {
			return data, true, nil
		}
	}

	if err != nil && !dockerapi.IsNotFound(err) {
		return dockerapi.Volume{}, false, err
	}

	// Stale: forget it, so the next placement fills a fresh one rather than re-checking this.
	_ = s.docker.RemoveVolume(ctx, alias.Name)

	return dockerapi.Volume{}, false, nil
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

	err = s.pour(ctx, data.Name, pack)
	if err != nil {
		_ = s.docker.RemoveVolume(context.WithoutCancel(ctx), data.Name)

		return dockerapi.Volume{}, err
	}

	return s.publish(ctx, digest, data)
}

func (s *plusSession) pour(ctx context.Context, volume string, pack func(io.Writer) error) error {
	holder, err := s.docker.CreateHolder(ctx, "steps-fill-"+randomSuffix(), treedigest.Image, shell.OwnershipLabels(), []string{volume + ":" + holderMount})
	if err != nil {
		return fmt.Errorf("%w", err)
	}
	defer func() { _ = s.docker.RemoveContainer(context.WithoutCancel(ctx), holder) }()

	reader, writer := io.Pipe()

	go func() { writer.CloseWithError(pack(writer)) }()

	err = s.docker.PutArchive(ctx, holder, holderMount, &byteCounter{r: reader, n: &s.sent})

	_ = reader.CloseWithError(io.ErrClosedPipe)

	if err != nil {
		return fmt.Errorf("sending a tree: %w", err)
	}

	return nil
}

// publish names data by digest. CreateVolume answers an existing name with that volume, so a lost race shows up as an alias pointing elsewhere.
func (s *plusSession) publish(ctx context.Context, digest string, data dockerapi.Volume) (dockerapi.Volume, error) {
	labels := shell.OwnershipLabels()
	labels[cacheLabel] = "alias"
	labels[cacheData] = data.Name
	labels[cacheDigest] = digest

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

// digestOf runs treedigest's script against a volume on the worker, read-only and with no network.
func (s *plusSession) digestOf(ctx context.Context, volume string) (string, error) {
	code, stdout, stderr, err := s.docker.RunOnce(ctx, dockerapi.ContainerSpec{
		Image:   treedigest.Image,
		Cmd:     []string{"sh", "-c", treedigest.Script, "sh", holderMount},
		Name:    "steps-digest-" + randomSuffix(),
		Labels:  shell.OwnershipLabels(),
		Network: "none",
		Mounts:  []string{volume + ":" + holderMount + ":ro"},
	})
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	if code != 0 {
		return "", fmt.Errorf("%w (exit %d): %s", errDigestRunFailed, code, stderr)
	}

	return stdout, nil
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

	child, err := s.docker.CreateVolume(ctx, "steps-in-"+randomSuffix(), shell.OwnershipLabels(),
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
func (s *plusSession) hold(ctx context.Context, volume string) (string, error) {
	digest, err := s.digestOf(ctx, volume)
	if err != nil {
		return "", err
	}

	data, err := s.docker.InspectVolume(ctx, volume)
	if err != nil {
		return "", fmt.Errorf("%w", err)
	}

	kept, err := s.publish(ctx, digest, data)
	if err != nil {
		return "", err
	}

	// An identical tree already held elsewhere wins the name; this volume then goes with the session.
	if kept.Name == volume {
		s.kept[volume] = true
	}

	return digest, nil
}

// pullPlus brings a held tree home from a docker+ worker and checks it against the digest it was asked for.
func pullPlus(ctx context.Context, worker Worker, digest, dst string) (int64, error) {
	s := &plusSession{worker: worker, kept: map[string]bool{}}
	//nolint:contextcheck // close runs under its own bound
	defer func() { _ = s.close() }()

	_, err := s.dialDaemon(ctx)
	if err != nil {
		return 0, err
	}

	data, hit, err := s.cached(ctx, digest)
	if err != nil {
		return 0, err
	}

	if !hit {
		return 0, fmt.Errorf("%s: %w", digest, errNotHeld)
	}

	holder, err := s.docker.CreateHolder(ctx, "steps-pull-"+randomSuffix(), treedigest.Image, shell.OwnershipLabels(), []string{data.Name + ":" + holderMount + ":ro"})
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
