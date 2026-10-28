//go:build !unix

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// noFollow is not available here; reads compare the opened file with an
// Lstat of the entry instead.
const noFollow = 0

// verifyDir checks that the opened cache directory is a directory. On this
// platform the per-user cache location's access control lists keep other
// users out.
func verifyDir(_ *os.File, fi fs.FileInfo, name string) error {
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", name)
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
