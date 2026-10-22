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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// runDir holds the real HOME of each session, out of the persona's reach.
const runDir = "/run/chaperone"

const fuseMagic = 0x65735546

// bind is one directory below HOME that the persona sees as the human's.
type bind struct {
	rel string
	ro  bool
}

// session runs cmd as the persona in a mount namespace of its own. HOME is
// the real one; each bind shows a directory of it through bindfs: the
// human's files there show as the persona's, and what the persona creates
// lands owned by the human. Afterwards the session's persona processes are
// killed and bindfs stops. path finds the command.
// ponytail: bindfs (FUSE) until the site kernel maps IDs on its HOME
// filesystem (idmapped mounts; CephFS since Linux 6.7).
func session(human, persona *user.User, home string, binds []bind, cmd *exec.Cmd, path string) (int, error) {
	// Mount namespaces and no_new_privs are per thread: everything from
	// here runs, and forks, on this one.
	runtime.LockOSThread()
	if err := syscall.Unshare(syscall.CLONE_NEWNS); err != nil {
		return 0, err
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return 0, err
	}
	if len(binds) > 0 {
		stop, err := mapBinds(human, persona, home, binds)
		if err != nil {
			return 0, err
		}
		defer stop()
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
			return 0, fmt.Errorf("%w (can the persona enter %s? chaperone bind it, or share it through a group)", err, cmd.Dir)
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

// mapBinds starts bindfs on the real HOME, in runDir (root only), and binds
// each directory of that view over its place in HOME. stop undoes it.
func mapBinds(human, persona *user.User, home string, binds []bind) (stop func(), err error) {
	var undo []func()
	stop = func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
	defer func() {
		if err != nil {
			stop()
		}
	}()
	if err := os.MkdirAll(runDir, 0700); err != nil {
		return nil, err
	}
	// src: the real HOME; view: bindfs on an empty directory (FUSE 2 refuses
	// a non-empty one). bindfs over its own source deadlocks.
	dir, err := os.MkdirTemp(runDir, "s")
	if err != nil {
		return nil, err
	}
	src, view := dir+"/src", dir+"/view"
	for _, d := range []string{src, view} {
		if err := os.Mkdir(d, 0700); err != nil {
			return nil, err
		}
	}
	// Never RemoveAll: src holds the human's HOME until it is unmounted.
	undo = append(undo, func() { os.Remove(src); os.Remove(view); os.Remove(dir) })
	if err := syscall.Mount(home, src, "", syscall.MS_BIND, ""); err != nil {
		return nil, err
	}
	undo = append(undo, func() { syscall.Unmount(src, syscall.MNT_DETACH) })
	hgrp, err := user.LookupGroupId(human.Gid)
	if err != nil {
		return nil, err
	}
	pgrp, err := user.LookupGroupId(persona.Gid)
	if err != nil {
		return nil, err
	}
	// Locks reach the real files, so the human and parallel sessions see
	// them; lock forwarding needs --multithreaded.
	bindfs := exec.Command("bindfs", "-f", "-o", "allow_other,default_permissions",
		"--map="+human.Username+"/"+persona.Username+":@"+hgrp.Name+"/@"+pgrp.Name,
		"--enable-lock-forwarding", "--multithreaded", src, view)
	bindfs.Env = []string{"PATH=" + safePath}
	bindfs.Stderr = os.Stderr
	bindfs.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := bindfs.Start(); err != nil {
		return nil, fmt.Errorf("bindfs: %w", err)
	}
	exited := make(chan struct{})
	go func() { bindfs.Wait(); close(exited) }()
	undo = append(undo, func() { bindfs.Process.Signal(syscall.SIGTERM); <-exited })
	if err := mounted(view, exited); err != nil {
		return nil, err
	}
	// Parents before children; a missing directory is skipped. Directories
	// only: a rename onto a mounted file fails (EBUSY).
	slices.SortFunc(binds, func(a, b bind) int { return strings.Compare(a.rel, b.rel) })
	const keep = syscall.MS_NOSUID | syscall.MS_NODEV
	for _, b := range binds {
		p := filepath.Join(home, b.rel)
		if fi, err := os.Lstat(p); err != nil || !fi.IsDir() {
			continue // symlinks too: a bind never leaves its place
		}
		if err := syscall.Mount(view+"/"+b.rel, p, "", syscall.MS_BIND, ""); err != nil {
			return nil, fmt.Errorf("bind %s: %w", b.rel, err)
		}
		undo = append(undo, func() { syscall.Unmount(p, syscall.MNT_DETACH) })
		if b.ro {
			if err := syscall.Mount("", p, "", syscall.MS_BIND|syscall.MS_REMOUNT|keep|syscall.MS_RDONLY, ""); err != nil {
				return nil, fmt.Errorf("bind %s read-only: %w", b.rel, err)
			}
		}
	}
	return stop, nil
}

// mounted waits for bindfs to show up on dir.
func mounted(dir string, exited chan struct{}) error {
	for range 100 {
		var st syscall.Statfs_t
		if syscall.Statfs(dir, &st) == nil && st.Type == fuseMagic {
			return nil
		}
		select {
		case <-exited:
			return errors.New("bindfs failed to mount HOME")
		case <-time.After(50 * time.Millisecond):
		}
	}
	return errors.New("bindfs did not mount HOME in time")
}

// killSession kills the persona's processes left in this session's mount
// namespace (daemons the command started), not those of parallel sessions.
// ponytail: a process that made a namespace of its own escapes this, but it
// loses HOME when bindfs stops.
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
