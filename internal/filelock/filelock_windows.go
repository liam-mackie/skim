//go:build windows

package filelock

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// Locks cover the whole file: offset 0 through the maximum 64-bit length.
const allBytes = ^uint32(0)

func lockFile(f *os.File, flags uint32) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), flags|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, allBytes, allBytes, new(windows.Overlapped))
}

// TryLock acquires a shared or exclusive LockFileEx lock without waiting.
func TryLock(f *os.File, exclusive bool) error {
	var flags uint32
	if exclusive {
		flags = windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	err := lockFile(f, flags)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrWouldBlock
	}
	return err
}

// Downgrade atomically converts the caller's exclusive lock to a shared lock.
// LockFileEx lets the holder of an exclusive range add a shared lock on it, and
// documents that the first of the two required unlocks releases the exclusive
// one. No other holder can acquire the range in between.
func Downgrade(f *os.File) error {
	if err := lockFile(f, 0); err != nil {
		return err
	}
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, allBytes, allBytes, new(windows.Overlapped))
}

// Unlock releases one lock held on f's file object (shared by its duplicates).
func Unlock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, allBytes, allBytes, new(windows.Overlapped))
}

// Dup returns a non-inheritable duplicate handle. Locks belong to the file
// object, which is released only when its last handle closes, so closing either
// file alone does not release the lock.
func Dup(f *os.File) (*os.File, error) {
	process := windows.CurrentProcess()
	var duplicate windows.Handle
	if err := windows.DuplicateHandle(process, windows.Handle(f.Fd()), process, &duplicate, 0, false, windows.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(duplicate), f.Name()), nil
}

// OpenNoFollow is os.OpenFile that fails if the final path element is a symlink,
// junction, or other reparse point. FILE_FLAG_OPEN_REPARSE_POINT (passed through
// the high bits of flag, as syscall.Open supports) opens the reparse point itself
// instead of its target, so the check below is on the opened object, not a race.
func OpenNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	file, err := os.OpenFile(path, flag|syscall.FILE_FLAG_OPEN_REPARSE_POINT, perm)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		file.Close()
		return nil, &os.PathError{Op: "open", Path: path, Err: errors.New("path is a symbolic link or reparse point")}
	}
	return file, nil
}

// SyncDir is a no-op: Windows cannot flush a directory handle (FlushFileBuffers
// fails), and NTFS journals metadata, making MoveFileEx the commit point.
func SyncDir(string) error { return nil }
