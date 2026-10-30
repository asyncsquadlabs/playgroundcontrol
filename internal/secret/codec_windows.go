//go:build windows

package secret

import (
	"errors"
	"fmt"
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	dpapiEntropy = "PlaygroundControl/DeepSeek/api-key/v1"
	dpapiFlags   = windows.CRYPTPROTECT_UI_FORBIDDEN | windows.CRYPTPROTECT_LOCAL_MACHINE
)

// DefaultCodec returns the production Windows protector for stored secrets.
func DefaultCodec() Codec {
	return dpapiCodec{}
}

type dpapiCodec struct{}

func (dpapiCodec) Seal(plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return []byte{}, nil
	}
	if uint64(len(plaintext)) > math.MaxUint32 {
		return nil, errors.New("secret is too large to protect")
	}
	entropy := secretBlob([]byte(dpapiEntropy))
	in := secretBlob(plaintext)
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, &entropy, 0, nil, dpapiFlags, &out); err != nil {
		return nil, fmt.Errorf("protect secret with DPAPI: %w", err)
	}
	defer func() {
		if out.Data != nil {
			_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
		}
	}()
	return copyBlob(out), nil
}

func (dpapiCodec) Open(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) == 0 {
		return []byte{}, nil
	}
	if uint64(len(ciphertext)) > math.MaxUint32 {
		return nil, errors.New("protected secret is too large")
	}
	entropy := secretBlob([]byte(dpapiEntropy))
	in := secretBlob(ciphertext)
	var out windows.DataBlob
	var name *uint16
	if err := windows.CryptUnprotectData(&in, &name, &entropy, 0, nil, dpapiFlags, &out); err != nil {
		return nil, fmt.Errorf("unprotect secret with DPAPI: %w", err)
	}
	defer func() {
		if out.Data != nil {
			_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
		}
		if name != nil {
			_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(name)))
		}
	}()
	return copyBlob(out), nil
}

func secretBlob(data []byte) windows.DataBlob {
	if len(data) == 0 {
		return windows.DataBlob{}
	}
	return windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
}

func copyBlob(blob windows.DataBlob) []byte {
	if blob.Size == 0 || blob.Data == nil {
		return []byte{}
	}
	return append([]byte(nil), unsafe.Slice(blob.Data, int(blob.Size))...)
}
