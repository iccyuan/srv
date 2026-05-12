//go:build windows

package session

import (
	"os"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"srv/internal/srvutil"
)

// platformID walks the process tree from os.Getpid() upward, skipping
// any ancestor whose exe is in intermediateExes (cmd.exe shim,
// python.exe launcher), and returns the first "real" ancestor's pid
// as string. This gives a stable id across srv invocations from the
// same shell.
func platformID() string {
	tree := walkProcesses()
	pid := uint32(os.Getpid())
	for i := 0; i < 20; i++ {
		entry, ok := tree[pid]
		if !ok {
			return srvutil.IntToStr(os.Getppid())
		}
		ppid := entry.parent
		if ppid == 0 {
			return srvutil.IntToStr(os.Getppid())
		}
		parent, ok := tree[ppid]
		if !ok {
			return srvutil.UintToStr(ppid)
		}
		if !intermediateExes[strings.ToLower(parent.exe)] {
			return srvutil.UintToStr(ppid)
		}
		pid = ppid
	}
	return srvutil.IntToStr(os.Getppid())
}

type procEntry struct {
	parent uint32
	exe    string
}

// walkProcesses snapshots all processes via Toolhelp32 and returns a
// map keyed by pid -> {parent_pid, exe_name}.
func walkProcesses() map[uint32]procEntry {
	out := map[uint32]procEntry{}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return out
	}
	defer windows.CloseHandle(snap)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snap, &entry); err != nil {
		return out
	}
	for {
		out[entry.ProcessID] = procEntry{
			parent: entry.ParentProcessID,
			exe:    windows.UTF16ToString(entry.ExeFile[:]),
		}
		if err := windows.Process32Next(snap, &entry); err != nil {
			break
		}
	}
	return out
}
