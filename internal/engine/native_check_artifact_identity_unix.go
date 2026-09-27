//go:build !windows

package engine

import (
	"os"
	"syscall"
)

func nativeArtifactUnsafeInfo(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeIrregular != 0 {
		return true
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return !ok || stat.Nlink != 1
}

func nativeArtifactOpenedSafe(f *os.File) bool { return f != nil }
