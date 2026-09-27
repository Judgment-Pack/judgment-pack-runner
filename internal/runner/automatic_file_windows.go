//go:build windows

package runner

import "os"

func openAutomaticFile(root *os.Root, name string) (*os.File, error) { return root.Open(name) }
