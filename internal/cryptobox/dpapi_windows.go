//go:build windows

package cryptobox

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

const cryptProtectUIForbidden = 0x1

type dataBlob struct {
	cbData uint32
	pbData *byte
}

var (
	crypt32            = syscall.NewLazyDLL("crypt32.dll")
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	cryptProtectData   = crypt32.NewProc("CryptProtectData")
	cryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	localFree          = kernel32.NewProc("LocalFree")
)

func protectMasterKey(plain []byte) ([]byte, error)        { return dpapiCall(plain, true) }
func unprotectMasterKey(ciphertext []byte) ([]byte, error) { return dpapiCall(ciphertext, false) }

func dpapiCall(input []byte, protect bool) ([]byte, error) {
	if len(input) == 0 {
		return nil, fmt.Errorf("DPAPI: dados vazios")
	}
	in := dataBlob{cbData: uint32(len(input)), pbData: &input[0]}
	var out dataBlob
	proc := cryptUnprotectData
	args := []uintptr{uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, cryptProtectUIForbidden, uintptr(unsafe.Pointer(&out))}
	if protect {
		proc = cryptProtectData
	}
	ok, _, callErr := proc.Call(args...)
	runtime.KeepAlive(input)
	if ok == 0 {
		return nil, fmt.Errorf("DPAPI: %w", callErr)
	}
	defer localFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	if out.cbData == 0 || out.pbData == nil {
		return nil, fmt.Errorf("DPAPI: resposta vazia")
	}
	return append([]byte(nil), unsafe.Slice(out.pbData, int(out.cbData))...), nil
}
