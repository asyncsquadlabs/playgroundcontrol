// Package secret seals small secrets before they are persisted. The Windows
// implementation uses machine-scoped DPAPI; portable builds use an identity
// codec so unit tests can run outside Windows.
package secret

// Codec protects a secret at rest.
type Codec interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(ciphertext []byte) ([]byte, error)
}
