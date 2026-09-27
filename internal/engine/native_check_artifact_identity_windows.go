//go:build windows

package engine

import (
	"os"

	"golang.org/x/sys/windows"
)

func nativeArtifactUnsafeInfo(info os.FileInfo) bool {
	return info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeIrregular != 0
}

func nativeArtifactOpenedSafe(f *os.File) bool {
	if f == nil {
		return false
	}
	var data windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &data) != nil {
		return false
	}
	return data.NumberOfLinks == 1 && data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0
}
