//go:build !unix

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// secureDir makes sure dir is a real directory, creating it if needed. On
// this platform the per-user cache location's access control lists keep
// other users out.
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
	if fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return nil
}

// trustedFile reports why a cache file must not be read, or nil.
func trustedFile(fi fs.FileInfo) error {
	if !fi.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	return nil
}
