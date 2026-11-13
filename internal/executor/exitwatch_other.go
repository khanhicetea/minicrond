//go:build !linux

package executor

import "os"

func exitWatch(int) *os.File { return nil }

func awaitExit(*os.File) {}
