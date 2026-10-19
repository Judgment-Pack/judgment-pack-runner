//go:build !linux && !darwin && !freebsd

package runner

func manualAdmissionPlatformSupported() bool { return false }

func OpenInheritedManualAuthority(ManualAdmissionBinding) (ManualAuthority, error) {
	return nil, errManualProtocol
}
