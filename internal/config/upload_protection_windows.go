package config

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

type dpapi struct{}

// PlatformSecretProtector returns Windows user-scoped, non-interactive DPAPI.
func PlatformSecretProtector() SecretProtector {
	return dpapi{}
}

func (dpapi) Protect(plain []byte) ([]byte, error) {
	in := dataBlob(plain)
	var out windows.DataBlob

	err := windows.CryptProtectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)

	return copyDPAPIResult(out, err)
}

func (dpapi) Unprotect(wrapped []byte) ([]byte, error) {
	in := dataBlob(wrapped)
	var out windows.DataBlob

	err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &out)

	return copyDPAPIResult(out, err)
}

func dataBlob(data []byte) windows.DataBlob {
	return windows.DataBlob{
		Size: uint32(len(data)),
		Data: unsafe.SliceData(data),
	}
}

func copyDPAPIResult(out windows.DataBlob, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}

	data := unsafe.Slice(out.Data, out.Size)
	copyOfData := append([]byte(nil), data...)
	clear(data)
	_, _ = windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))

	return copyOfData, nil
}

func publishNewUploadConfig(from, to string) error {
	src, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}

	dest, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}

	err = windows.MoveFile(src, dest)
	if os.IsExist(err) {
		return os.ErrExist
	}

	return err
}
