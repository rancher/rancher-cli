//go:build windows

package envtestlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// exitWait is how long a terminated process is given to exit.
const exitWait = 10 * time.Second

// errSignalUnsupported is the error envtest returns for each process it stops:
// Go's os.Process.Signal on Windows accepts only Kill.
var errSignalUnsupported error = syscall.EWINDOWS

// childProcess is a running process whose parent is this process.
type childProcess struct {
	pid  uint32
	name string
}

// childProcesses lists the processes whose parent is this process, by the
// file name of their program.
func childProcesses() ([]childProcess, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	defer func() { _ = windows.CloseHandle(snap) }()

	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	self := uint32(os.Getpid())
	var children []childProcess
	for err = windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		if entry.ParentProcessID == self {
			children = append(children, childProcess{entry.ProcessID, windows.UTF16ToString(entry.ExeFile[:])})
		}
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, fmt.Errorf("listing processes: %w", err)
	}
	return children, nil
}

// killControlPlane terminates every etcd and kube-apiserver child of this
// process and waits for each to exit. It returns how many are stopped:
// terminated, or already exited.
func killControlPlane() (int, error) {
	children, err := childProcesses()
	if err != nil {
		return 0, err
	}
	var stopped int
	var errs []error
	for _, child := range children {
		if !isControlPlaneImage(child.name) {
			continue
		}
		ok, err := terminate(child)
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			stopped++
		}
	}
	return stopped, errors.Join(errs...)
}

// terminate ends one process and waits up to exitWait for it to exit. It
// returns true when the process is stopped: it was gone or had exited
// already, or it exited after TerminateProcess. A process whose program is
// no longer etcd or the kube-apiserver is not touched and gives false and no
// error: the child exited after the snapshot and its pid went to another
// process.
func terminate(child childProcess) (bool, error) {
	h, err := windows.OpenProcess(
		windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, child.pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("opening %s (pid %d): %w", child.name, child.pid, err)
	}
	defer func() { _ = windows.CloseHandle(h) }()

	event, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return false, fmt.Errorf("checking %s (pid %d): %w", child.name, child.pid, err)
	}
	if event == windows.WAIT_OBJECT_0 {
		return true, nil
	}

	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return false, fmt.Errorf("reading the program of %s (pid %d): %w", child.name, child.pid, err)
	}
	if !isControlPlaneImage(filepath.Base(windows.UTF16ToString(buf[:size]))) {
		return false, nil
	}

	if err := windows.TerminateProcess(h, 1); err != nil {
		return false, fmt.Errorf("terminating %s (pid %d): %w", child.name, child.pid, err)
	}
	event, err = windows.WaitForSingleObject(h, uint32(exitWait.Milliseconds()))
	if err != nil {
		return false, fmt.Errorf("waiting for %s (pid %d) to exit: %w", child.name, child.pid, err)
	}
	switch event {
	case windows.WAIT_OBJECT_0:
		return true, nil
	case uint32(windows.WAIT_TIMEOUT):
		return false, fmt.Errorf("%s (pid %d) did not exit within %s", child.name, child.pid, exitWait)
	}
	return false, fmt.Errorf("waiting for %s (pid %d) to exit: unexpected result %#x", child.name, child.pid, event)
}
