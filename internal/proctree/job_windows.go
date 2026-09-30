//go:build windows

package proctree

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Job is a Job Object. Processes assigned to it, and everything they start,
// stay in it: the job is created without JOB_OBJECT_LIMIT_BREAKAWAY_OK. The
// job kills its processes when its last handle closes, so nothing outlives
// the owning process by accident.
type Job struct {
	handle windows.Handle
}

func New() (*Job, error) {
	handle, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("creating job object: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		handle,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(handle)
		return nil, fmt.Errorf("configuring job object: %w", err)
	}
	return &Job{handle: handle}, nil
}

// Prepare makes cmd start suspended so Adopt can assign it before it runs
// any code or starts a child. Console programs get no console window.
func Prepare(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED | windows.CREATE_NO_WINDOW
}

// Adopt assigns a process started after Prepare to the job, then resumes it.
// If that fails, the still-suspended process is terminated.
func (j *Job) Adopt(p *os.Process) error {
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.Pid))
	if err != nil {
		_ = p.Kill()
		return fmt.Errorf("opening process %d: %w", p.Pid, err)
	}
	defer windows.CloseHandle(handle)
	if err := windows.AssignProcessToJobObject(j.handle, handle); err != nil {
		_ = windows.TerminateProcess(handle, 1)
		return fmt.Errorf("assigning process %d to job object: %w", p.Pid, err)
	}
	if err := resumeThreads(uint32(p.Pid)); err != nil {
		_ = windows.TerminateProcess(handle, 1)
		return fmt.Errorf("resuming process %d: %w", p.Pid, err)
	}
	return nil
}

func resumeThreads(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	resumed := 0
	for err = windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return err
		}
		_, err = windows.ResumeThread(thread)
		windows.CloseHandle(thread)
		if err != nil {
			return err
		}
		resumed++
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return err
	}
	if resumed == 0 {
		return fmt.Errorf("no threads found")
	}
	return nil
}

// processIDList is JOBOBJECT_BASIC_PROCESS_ID_LIST.
type processIDList struct {
	Assigned uint32
	Listed   uint32
	IDs      [1]uintptr
}

// PIDs lists the processes currently in the job.
func (j *Job) PIDs() ([]int, error) {
	capacity := 64
	for {
		buffer := make([]byte, unsafe.Offsetof(processIDList{}.IDs)+uintptr(capacity)*unsafe.Sizeof(uintptr(0)))
		list := (*processIDList)(unsafe.Pointer(&buffer[0]))
		err := windows.QueryInformationJobObject(
			j.handle,
			windows.JobObjectBasicProcessIdList,
			uintptr(unsafe.Pointer(list)),
			uint32(len(buffer)),
			nil,
		)
		if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
			return nil, err
		}
		if err == nil && list.Listed >= list.Assigned {
			ids := unsafe.Slice(&list.IDs[0], list.Listed)
			pids := make([]int, len(ids))
			for i, id := range ids {
				pids[i] = int(id)
			}
			return pids, nil
		}
		capacity = max(2*capacity, int(list.Assigned)+16)
	}
}

// Terminate ends every process in the job with exitCode.
func (j *Job) Terminate(exitCode uint32) error {
	return windows.TerminateJobObject(j.handle, exitCode)
}

// Close releases the job, killing anything still in it.
func (j *Job) Close() error {
	if j.handle == 0 {
		return nil
	}
	err := windows.CloseHandle(j.handle)
	j.handle = 0
	return err
}
