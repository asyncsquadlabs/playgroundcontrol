//go:build windows

package secret

import (
	"bytes"
	"testing"
)

func TestDPAPIRoundTripAndTamperDetection(t *testing.T) {
	codec := DefaultCodec()
	plaintext := []byte("sk-deepseek-test-key-1234")
	sealed, err := codec.Seal(plaintext)
	if err != nil {
		t.Skipf("DPAPI is unavailable in this environment: %v", err)
	}
	if len(sealed) == 0 || bytes.Contains(sealed, plaintext) {
		t.Fatalf("sealed secret is empty or still contains plaintext")
	}
	opened, err := codec.Open(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(opened, plaintext) {
		t.Fatalf("round trip = %q, want %q", opened, plaintext)
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := codec.Open(tampered); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}
