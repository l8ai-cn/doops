//go:build !linux && !darwin

package server

import "errors"

func exchangeWorkspaceDirectories(_, _ string) error {
	return errors.New("atomic workspace replacement is unsupported on this platform")
}
