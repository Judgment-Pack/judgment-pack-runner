//go:build linux || darwin || freebsd

package runner

import (
	"net"
	"os"
	"syscall"
)

func manualAdmissionPlatformSupported() bool { return true }

// OpenInheritedManualAuthority takes only the explicitly negotiated Unix stream
// socket. net.FileConn duplicates it with close-on-exec; the inherited original
// is marked first, then closed on every path. No descriptor reaches Runtime.
func OpenInheritedManualAuthority(binding ManualAdmissionBinding) (ManualAuthority, error) {
	if binding.FD != 3 {
		return nil, errManualProtocol
	}
	file := os.NewFile(uintptr(binding.FD), "manual-authority")
	if file == nil {
		return nil, errManualProtocol
	}
	defer file.Close()
	syscall.CloseOnExec(binding.FD)
	address, err := syscall.Getsockname(binding.FD)
	if err != nil {
		return nil, errManualProtocol
	}
	if _, ok := address.(*syscall.SockaddrUnix); !ok {
		return nil, errManualProtocol
	}
	kind, err := syscall.GetsockoptInt(binding.FD, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil || kind != syscall.SOCK_STREAM {
		return nil, errManualProtocol
	}
	conn, err := net.FileConn(file)
	if err != nil {
		return nil, errManualProtocol
	}
	return NewManualAuthority(conn, binding)
}
