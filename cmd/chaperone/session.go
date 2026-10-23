package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// session runs cmd as the persona in a mount namespace of its own, which
// marks the session's processes: afterwards the persona's processes left in
// it are killed, not those of parallel sessions. path finds the command.
func session(persona *user.User, cmd *exec.Cmd, path string) (int, error) {
	// Mount namespaces and no_new_privs are per thread: everything from
	// here runs, and forks, on this one.
	runtime.LockOSThread()
	if err := syscall.Unshare(syscall.CLONE_NEWNS); err != nil {
		return 0, err
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return 0, err
	}

	os.Setenv("PATH", path)
	var err error
	if cmd.Path, err = exec.LookPath(cmd.Args[0]); err != nil {
		return 0, err
	}
	// No setuid binary (sudo included) gains privileges below the persona.
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, 38 /* PR_SET_NO_NEW_PRIVS */, 1, 0, 0, 0, 0); e != 0 {
		return 0, e
	}
	sig := make(chan os.Signal, 1)
	// The terminal already sends SIGINT/SIGQUIT to the child's process group.
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT)
	if err := cmd.Start(); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return 0, fmt.Errorf("%w (can the persona enter %s? chaperone include it, or share it through a group)", err, cmd.Dir)
		}
		return 0, err
	}
	go func() {
		for s := range sig {
			if s == syscall.SIGTERM || s == syscall.SIGHUP {
				cmd.Process.Signal(s)
			}
		}
	}()
	cmd.Wait()
	killSession(persona.Uid)
	return exitStatus(cmd.ProcessState), nil
}

// killSession kills the persona's processes left in this session's mount
// namespace (daemons the command started), not those of parallel sessions.
// ponytail: a process that made a namespace of its own escapes this.
func killSession(uid string) {
	self, err := os.Readlink("/proc/thread-self/ns/mnt")
	if err != nil {
		return
	}
	for range 10 {
		left := false
		pids, _ := filepath.Glob("/proc/[0-9]*")
		for _, d := range pids {
			ns, err := os.Readlink(d + "/ns/mnt")
			if err != nil || ns != self {
				continue
			}
			var st syscall.Stat_t
			if syscall.Stat(d, &st) != nil || strconv.Itoa(int(st.Uid)) != uid {
				continue
			}
			pid, _ := strconv.Atoi(filepath.Base(d))
			syscall.Kill(pid, syscall.SIGKILL)
			left = true
		}
		if !left {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}
