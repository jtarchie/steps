package venue

import "context"

// gceFuncs holds gceClient's method values: gceClient in an interface keeps all of compute/v1 linked (~7MB) via its service field.
type gceFuncs struct {
	insert     func(ctx context.Context, project, zone, name, template string) error
	start      func(ctx context.Context, project, zone, name string) error
	stop       func(ctx context.Context, project, zone, name string) error
	del        func(ctx context.Context, project, zone, name string) error
	status     func(ctx context.Context, project, zone, name string) (string, error)
	addSSHKey  func(ctx context.Context, project, zone, name, entry string) error
	guestAttrs func(ctx context.Context, project, zone, name, path string) (map[string]string, error)
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
	}
}

func (f gceFuncs) InsertFromTemplate(ctx context.Context, project, zone, name, template string) error {
	return f.insert(ctx, project, zone, name, template)
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
