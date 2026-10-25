package venue

import (
	"context"
	"time"
)

// gceFuncs holds gceClient's method values: gceClient in an interface keeps all of compute/v1 linked (~7MB) via its service field.
type gceFuncs struct {
	insert     func(ctx context.Context, project, zone, name, template string, labels map[string]string) error
	start      func(ctx context.Context, project, zone, name string) error
	stop       func(ctx context.Context, project, zone, name string) error
	del        func(ctx context.Context, project, zone, name string) error
	status     func(ctx context.Context, project, zone, name string) (string, error)
	addSSHKey  func(ctx context.Context, project, zone, name, entry string) error
	guestAttrs func(ctx context.Context, project, zone, name, path string) (map[string]string, error)
	owner      func(ctx context.Context, project, zone, name string) (ownedInstance, error)
	setOwner   func(ctx context.Context, project, zone, name, value string) error
	listOwned  func(ctx context.Context, project, zone string) ([]ownedInstance, error)
	reclaimed  func(ctx context.Context, project, zone, name string, since time.Time) (string, error)
}

func newGCEFuncs(client *gceClient) gceFuncs {
	return gceFuncs{
		insert:     client.InsertFromTemplate,
		start:      client.Start,
		stop:       client.Stop,
		del:        client.Delete,
		status:     client.Status,
		addSSHKey:  client.AddSSHKey,
		guestAttrs: client.GuestAttributes,
		owner:      client.Owner,
		setOwner:   client.SetOwner,
		listOwned:  client.ListOwned,
		reclaimed:  client.Reclaimed,
	}
}

func (f gceFuncs) InsertFromTemplate(ctx context.Context, project, zone, name, template string, labels map[string]string) error {
	return f.insert(ctx, project, zone, name, template, labels)
}

func (f gceFuncs) Start(ctx context.Context, project, zone, name string) error {
	return f.start(ctx, project, zone, name)
}

func (f gceFuncs) Stop(ctx context.Context, project, zone, name string) error {
	return f.stop(ctx, project, zone, name)
}

func (f gceFuncs) Delete(ctx context.Context, project, zone, name string) error {
	return f.del(ctx, project, zone, name)
}

func (f gceFuncs) Status(ctx context.Context, project, zone, name string) (string, error) {
	return f.status(ctx, project, zone, name)
}

func (f gceFuncs) AddSSHKey(ctx context.Context, project, zone, name, entry string) error {
	return f.addSSHKey(ctx, project, zone, name, entry)
}

func (f gceFuncs) GuestAttributes(ctx context.Context, project, zone, name, path string) (map[string]string, error) {
	return f.guestAttrs(ctx, project, zone, name, path)
}

func (f gceFuncs) Owner(ctx context.Context, project, zone, name string) (ownedInstance, error) {
	return f.owner(ctx, project, zone, name)
}

func (f gceFuncs) SetOwner(ctx context.Context, project, zone, name, value string) error {
	return f.setOwner(ctx, project, zone, name, value)
}

func (f gceFuncs) ListOwned(ctx context.Context, project, zone string) ([]ownedInstance, error) {
	return f.listOwned(ctx, project, zone)
}

func (f gceFuncs) Reclaimed(ctx context.Context, project, zone, name string, since time.Time) (string, error) {
	return f.reclaimed(ctx, project, zone, name, since)
}
