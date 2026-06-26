//go:build windows

package main

import (
	"os"
)

func lockFile(f *os.File) error {
	return nil
}

func unlockFile(f *os.File) error {
	return nil
}
