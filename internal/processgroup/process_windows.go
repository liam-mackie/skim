//go:build windows

package processgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A Windows process group is a Job Object named after the leader's PID, so the
// group ID recorded by callers remains inspectable after a Skim restart. Every
// job is KILL_ON_JOB_CLOSE: if Skim dies, the kernel closes its handle and
// terminates every member, so no crash can leave a member running unobserved.

var (
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procCreateJobObjectW = kernel32.NewProc("CreateJobObjectW")
	procOpenJobObjectW   = kernel32.NewProc("OpenJobObjectW")
)

const jobObjectQuery = 0x0004 // JOB_OBJECT_QUERY

// jobObjectBasicAccountingInformation is JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
type jobObjectBasicAccountingInformation struct {
	TotalUserTime             int64
	TotalKernelTime           int64
	ThisPeriodTotalUserTime   int64
	ThisPeriodTotalKernelTime int64
	TotalPageFaultCount       uint32
	TotalProcesses            uint32
	ActiveProcesses           uint32
	TotalTerminatedProcesses  uint32
}

// jobs owns this process's job handles by group ID. Every use of a handle
// happens under the lock, so Wait cannot close a handle another call is using.
var jobs = struct {
	sync.Mutex
	handles map[int]windows.Handle
}{handles: map[int]windows.Handle{}}

func jobName(group int) (*uint16, error) {
	return windows.UTF16PtrFromString(`Local\skim-process-group-` + strconv.Itoa(group))
}

// Start starts cmd suspended, assigns it to a new kill-on-close Job Object, and
// only then resumes it, so no descendant can escape the group. CommandContext
// cancellation kills the entire job. Pair a successful Start with cmd.Wait, then
// Kill and Wait.
func Start(cmd *exec.Cmd) error {
	// CREATE_NO_WINDOW keeps console children of a GUI (or console-less) Skim
	// from flashing a console window; stdio is always redirected.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
	if cmd.Cancel != nil {
		cmd.Cancel = func() error { return Kill(cmd) }
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := adopt(cmd.Process.Pid); err != nil {
		// The leader never ran, so it has no descendants to clean up.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	return nil
}

func adopt(pid int) (err error) {
	name, err := jobName(pid)
	if err != nil {
		return err
	}
	// Called directly: x/sys/windows.CreateJobObject does not report
	// ERROR_ALREADY_EXISTS, and an existing job may hold unrelated processes.
	r, _, callErr := procCreateJobObjectW.Call(0, uintptr(unsafe.Pointer(name)))
	job := windows.Handle(r)
	if job == 0 {
		return fmt.Errorf("cannot create process group job object: %w", callErr)
	}
	if errors.Is(callErr, windows.ERROR_ALREADY_EXISTS) {
		windows.CloseHandle(job)
		return fmt.Errorf("process group job object for process %d already exists", pid)
	}
	defer func() {
		if err != nil {
			windows.CloseHandle(job) // Kill-on-close terminates any assigned member.
		}
	}()
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return fmt.Errorf("cannot configure process group job object: %w", err)
	}
	// The exec.Cmd still holds the process handle, so the PID cannot be reused.
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("cannot open started process: %w", err)
	}
	err = windows.AssignProcessToJobObject(job, process)
	windows.CloseHandle(process)
	if err != nil {
		return fmt.Errorf("cannot assign process to its job object: %w", err)
	}
	jobs.Lock()
	jobs.handles[pid] = job
	jobs.Unlock()
	if err := resume(pid); err != nil {
		jobs.Lock()
		delete(jobs.handles, pid)
		_ = windows.TerminateJobObject(job, 1)
		jobs.Unlock()
		return err
	}
	return nil
}

// resume starts the threads of a process created with CREATE_SUSPENDED. exec
// closes the primary thread handle, so the thread is found by snapshot; resuming
// a thread that was not suspended is harmless.
func resume(pid int) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return fmt.Errorf("cannot enumerate threads of started process: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	resumed := 0
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != uint32(pid) {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return fmt.Errorf("cannot open thread of started process: %w", err)
		}
		_, err = windows.ResumeThread(thread)
		windows.CloseHandle(thread)
		if err != nil {
			return fmt.Errorf("cannot resume started process: %w", err)
		}
		resumed++
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return fmt.Errorf("cannot enumerate threads of started process: %w", err)
	}
	if resumed == 0 {
		return errors.New("started process has no thread to resume")
	}
	return nil
}

func activeProcesses(job windows.Handle) (uint32, error) {
	var info jobObjectBasicAccountingInformation
	err := windows.QueryInformationJobObject(job, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
	return info.ActiveProcesses, err
}

// Kill terminates every process in the command's job, including orphaned tools.
// Wait must still be called after reaping the direct child.
func Kill(cmd *exec.Cmd) error {
	var err error
	jobs.Lock()
	job, ok := jobs.handles[cmd.Process.Pid]
	if ok {
		if active, queryErr := activeProcesses(job); queryErr == nil && active == 0 {
			err = os.ErrProcessDone
		} else {
			err = windows.TerminateJobObject(job, 1)
		}
	}
	jobs.Unlock()
	killErr := cmd.Process.Kill()
	if !ok {
		return killErr
	}
	return err
}

// Wait runs after the direct child has been reaped. Termination is asynchronous:
// a member may still have a workspace file open until it has fully exited, so
// Wait blocks until the job has no active process, then releases the job.
func Wait(cmd *exec.Cmd) {
	group := cmd.Process.Pid
	for {
		jobs.Lock()
		job, ok := jobs.handles[group]
		if !ok {
			jobs.Unlock()
			return
		}
		if active, err := activeProcesses(job); err == nil && active == 0 {
			delete(jobs.handles, group)
			jobs.Unlock()
			windows.CloseHandle(job)
			return
		}
		// Fail closed if the job cannot be inspected. Never release a workspace
		// merely because a timeout elapsed while a member can write.
		_ = windows.TerminateJobObject(job, 1)
		jobs.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
}

// Stopped reports whether no executing process remains in a process group. It
// never terminates anything. A job that no longer exists is stopped: a job
// object lives while any handle or member does, and kill-on-close terminated
// the members when the Skim process that owned the handle exited.
func Stopped(group int) (bool, error) {
	jobs.Lock()
	if job, ok := jobs.handles[group]; ok {
		active, err := activeProcesses(job)
		jobs.Unlock()
		return err == nil && active == 0, err
	}
	jobs.Unlock()
	name, err := jobName(group)
	if err != nil {
		return false, err
	}
	r, _, callErr := procOpenJobObjectW.Call(jobObjectQuery, 0, uintptr(unsafe.Pointer(name)))
	job := windows.Handle(r)
	if job == 0 {
		if errors.Is(callErr, windows.ERROR_FILE_NOT_FOUND) {
			return true, nil
		}
		return false, fmt.Errorf("cannot inspect process group job object: %w", callErr)
	}
	defer windows.CloseHandle(job)
	active, err := activeProcesses(job)
	if err != nil {
		return false, err
	}
	return active == 0, nil
}
