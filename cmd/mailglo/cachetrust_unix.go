//go:build unix

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// secureDir makes sure dir is a directory owned by this user that no one
// else can write, creating it if needed. A directory that is ours but too
// open is tightened to 0700.
func secureDir(dir string) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		fi, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if !ownedByMe(fi) {
		return fmt.Errorf("%s belongs to another user", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("%s is open to other users: %w", dir, err)
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
