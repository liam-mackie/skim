// Package filelock provides whole-file advisory locks, lock-sharing descriptor
// duplication, symlink-refusing opens, and directory durability for Unix and
// Windows.
//
// Locks belong to the open file (Unix open file description, Windows file
// object), not to the process: two independent opens of the same path conflict
// even inside one process, while duplicates returned by Dup share one lock.
// Lock files must be used only for locking; Windows locks are mandatory, so
// their bytes cannot be read or written by other handles while locked.
package filelock

import "errors"

// ErrWouldBlock reports that TryLock found a conflicting lock.
var ErrWouldBlock = errors.New("file is locked by another holder")
