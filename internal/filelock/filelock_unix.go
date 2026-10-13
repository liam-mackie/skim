//go:build unix

package filelock

import (
	"os"
	"syscall"
)

// TryLock acquires a shared or exclusive flock without waiting.
func TryLock(f *os.File, exclusive bool) error {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
	if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
		return ErrWouldBlock
	}
	return err
}

// Downgrade converts the caller's exclusive lock to a shared lock.
func Downgrade(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_SH) }

// Unlock releases the lock held through f (and any duplicate of it).
func Unlock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }

// Dup returns a close-on-exec duplicate sharing f's open file description, and
// therefore its lock: closing either file alone does not release the lock.
func Dup(f *os.File) (*os.File, error) {
	// Hold ForkLock so a concurrent exec cannot inherit the descriptor before
	// close-on-exec is set.
	syscall.ForkLock.RLock()
	fd, err := syscall.Dup(int(f.Fd()))
	if err == nil {
		syscall.CloseOnExec(fd)
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), f.Name()), nil
}

// OpenNoFollow is os.OpenFile that fails if the final path element is a symlink.
func OpenNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(path, flag|syscall.O_NOFOLLOW, perm)
}

// SyncDir makes completed renames and creations in a directory durable.
func SyncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
