package store

import "context"

// Credentials is what a pipeline keeps secret: bytes SEALED above the store (internal/sealbox), which the store holds and cannot read. A copy of the database is a copy of ciphertext.
type Credentials interface {
	// Credential is the sealed value saved under name; found is false when there is none.
	Credential(ctx context.Context, name string) (sealed []byte, found bool, err error)
	// SaveCredential replaces whatever name held.
	SaveCredential(ctx context.Context, name string, sealed []byte) error
	// CredentialScope is what a sealed value is bound to, so a row copied onto another pipeline does not open: stable across a rename, never reused after a destroy.
	CredentialScope() string
}
