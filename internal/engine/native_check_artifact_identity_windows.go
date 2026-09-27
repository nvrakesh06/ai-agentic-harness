//go:build windows

package engine

import (
	"fmt"
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

func nativeArtifactPathSafe(path string, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeIrregular != 0 || (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		return os.ErrPermission
	}
	handle, err := windows.CreateFile(windows.StringToUTF16Ptr(path), windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	var data windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(handle, &data); err != nil {
		return err
	}
	if data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return fmt.Errorf("native artifact path is a Windows reparse point")
	}
	return nil
}
