//go:build !linux && !darwin && !freebsd

package runner

import (
	"io"
	"os"
)

func readManualDemand(ExecutorOwnership) (*ManualExecutionDemand, error) {
	return nil, errManualProtocol
}
func adoptManualPreparationLease(_ ExecutorOwnership, _ string, file *os.File) (*ExecutorLease, error) {
	if file != nil {
		file.Close()
	}
	return nil, errManualProtocol
}
func manualPreparationInstanceLock(*ExecutorLease) (*os.File, error) { return nil, errManualProtocol }
func readManualStateFile(*ExecutorLease, string, int64) ([]byte, error) {
	return nil, errManualProtocol
}
func writeManualPrepareMarker(*ExecutorLease, []byte) error      { return errManualProtocol }
func verifyManualDatabasePath(*ExecutorLease, os.FileInfo) error { return errManualProtocol }
func manualDatabaseTarget(*ExecutorLease) (string, *os.File, error) {
	return "", nil, errManualProtocol
}
func manualProcessDatabaseTarget(*ExecutorLease) (string, io.Closer, error) {
	return "", nil, errManualProtocol
}
