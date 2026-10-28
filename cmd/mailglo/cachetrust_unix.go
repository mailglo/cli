//go:build unix

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// noFollow keeps an open from following a link at the final path element.
// os.Root may still resolve links that stay inside the directory, so reads
// also compare the opened file with an Lstat of the entry.
const noFollow = syscall.O_NOFOLLOW

// verifyDir checks the opened cache directory: it must belong to this user,
// and no one else may write to it. A directory that is ours but too open is
// tightened to 0700 through the open handle.
func verifyDir(d *os.File, fi fs.FileInfo, name string) error {
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", name)
	}
	if !ownedByMe(fi) {
		return fmt.Errorf("%s belongs to another user", name)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := d.Chmod(0o700); err != nil {
			return fmt.Errorf("%s is open to other users: %w", name, err)
		}
	}
	return nil
}

// trustedFile reports why a cache file must not be read, or nil.
func trustedFile(fi fs.FileInfo) error {
	switch {
	case !fi.Mode().IsRegular():
		return errors.New("not a regular file")
	case !ownedByMe(fi):
		return errors.New("belongs to another user")
	case fi.Mode().Perm()&0o022 != 0:
		return errors.New("writable by other users")
	}
	return nil
}

func ownedByMe(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
