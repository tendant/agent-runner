//go:build !linux

package isobox

import "errors"

func setNproc(uint64) error { return errors.New("only supported on Linux (gVisor)") }
