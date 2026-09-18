package store

import "context"

// Deliveries is what a webhook resource receives: each delivery is a version, and its payload is what a get of that version writes out.
type Deliveries interface {
	// RecordDelivery files a delivery as the newest version of resourceName, keeps its payload, and dispatches it, all in one transaction — a sender that was answered 2xx has had all of it happen. A version already recorded (a redelivery) changes nothing and reports false.
	RecordDelivery(ctx context.Context, resourceName string, delivery Delivery, dispatch Dispatch, limit int) (bool, error)
	// Delivery is the payload recorded with a version; found is false once version_history: has pruned it.
	Delivery(ctx context.Context, resourceName, versionJSON string) (Delivery, bool, error)
}

// Delivery is one webhook delivery as recorded.
type Delivery struct {
	Version map[string]any
	Body    []byte
	Headers map[string]string
}

// Dispatch is what recording a delivery sets in motion: the jobs it queues, and the resource's current version moving to it. Held records the delivery and does neither — a paused pipeline — so the first poll after unpause finds a current version behind the newest delivery and dispatches it then.
type Dispatch struct {
	Jobs []string
	Held bool
}
