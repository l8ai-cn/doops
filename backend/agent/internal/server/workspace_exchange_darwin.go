package server

import "golang.org/x/sys/unix"

func exchangeWorkspaceDirectories(source, destination string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, source, unix.AT_FDCWD, destination, unix.RENAME_SWAP)
}
