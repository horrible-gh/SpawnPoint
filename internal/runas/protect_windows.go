//go:build windows

package runas

import (
	"encoding/base64"
	"errors"
	"runtime"
	"syscall"
	"unsafe"
)

var crypt32 = syscall.NewLazyDLL("crypt32.dll")
var protectData = crypt32.NewProc("CryptProtectData")
var unprotectData = crypt32.NewProc("CryptUnprotectData")
var localFree = syscall.NewLazyDLL("kernel32.dll").NewProc("LocalFree")

type dataBlob struct {
	Size uint32
	Data *byte
}

func blob(b []byte) dataBlob {
	if len(b) == 0 {
		return dataBlob{}
	}
	return dataBlob{uint32(len(b)), &b[0]}
}
func dpapi(proc *syscall.LazyProc, b, entropy []byte) ([]byte, error) {
	in, extra := blob(b), blob(entropy)
	var out dataBlob
	r, _, e := proc.Call(uintptr(unsafe.Pointer(&in)), 0, uintptr(unsafe.Pointer(&extra)), 0, 0, 1, uintptr(unsafe.Pointer(&out)))
	runtime.KeepAlive(b)
	runtime.KeepAlive(entropy)
	if r == 0 {
		return nil, e
	}
	defer localFree.Call(uintptr(unsafe.Pointer(out.Data)))
	if out.Size > 65536 {
		return nil, errors.New("unexpected protected credential size")
	}
	v := unsafe.Slice(out.Data, int(out.Size))
	result := append([]byte(nil), v...)
	wipe(v)
	return result, nil
}
func seal(_ string, b, entropy []byte) (string, error) {
	x, e := dpapi(protectData, b, entropy)
	if e != nil {
		return "", e
	}
	defer wipe(x)
	return base64.StdEncoding.EncodeToString(x), nil
}
func unseal(_ string, s string, entropy []byte) ([]byte, error) {
	b, e := base64.StdEncoding.DecodeString(s)
	if e != nil {
		return nil, e
	}
	return dpapi(unprotectData, b, entropy)
}
