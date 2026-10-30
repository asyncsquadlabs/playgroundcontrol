//go:build !windows

package secret

// DefaultCodec is used by portable tests. The production target is Windows,
// where DefaultCodec uses DPAPI. Non-Windows builds are not supported for
// deployment.
func DefaultCodec() Codec {
	return identityCodec{}
}

type identityCodec struct{}

func (identityCodec) Seal(plaintext []byte) ([]byte, error) {
	return append([]byte(nil), plaintext...), nil
}

func (identityCodec) Open(ciphertext []byte) ([]byte, error) {
	return append([]byte(nil), ciphertext...), nil
}
