//go:build !windows

package cryptobox

import "errors"

var errDPAPIUnavailable = errors.New("cryptobox: DPAPI so esta disponivel no Windows")

func protectMasterKey([]byte) ([]byte, error)   { return nil, errDPAPIUnavailable }
func unprotectMasterKey([]byte) ([]byte, error) { return nil, errDPAPIUnavailable }
