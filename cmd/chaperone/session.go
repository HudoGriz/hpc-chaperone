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
	"strings"
	"syscall"
	"time"
)

// runDir holds the real HOME of each session, out of the persona's reach.
const runDir = "/run/chaperone"

const fuseMagic = 0x65735546

// session runs cmd as the persona in a mount namespace of its own. HOME is
// the human's, seen through bindfs: the human's files show as the
// persona's, and what the persona creates lands owned by the human. The
// profile's layer goes on top. Afterwards the session's persona processes
// are killed and bindfs stops, so nothing keeps HOME access. path finds
// the command.
// ponytail: bindfs (FUSE) until the site kernel maps IDs on its HOME
// filesystem (idmapped mounts; CephFS since Linux 6.7).
func session(human, persona *user.User, pr Persona, cmd *exec.Cmd, path string) (int, error) {
	// Mount namespaces and no_new_privs are per thread: everything from
	// here runs, and forks, on this one.
	runtime.LockOSThread()
	if err := syscall.Unshare(syscall.CLONE_NEWNS); err != nil {
		return 0, err
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return 0, err
	}
	home, err := filepath.EvalSymlinks(human.HomeDir)
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(runDir, 0700); err != nil {
		return 0, err
	}
	// src: the real HOME; view: bindfs on an empty directory (FUSE 2 refuses
	// a non-empty one), then moved over HOME.
	dir, err := os.MkdirTemp(runDir, "s")
	if err != nil {
		return 0, err
	}
	src, view := dir+"/src", dir+"/view"
	for _, d := range []string{src, view} {
		if err := os.Mkdir(d, 0700); err != nil {
			return 0, err
		}
	}
	// Never RemoveAll: src holds the human's HOME until it is unmounted.
	defer func() { os.Remove(src); os.Remove(view); os.Remove(dir) }()
	if err := syscall.Mount(home, src, "", syscall.MS_BIND, ""); err != nil {
		return 0, err
	}
	defer syscall.Unmount(src, syscall.MNT_DETACH)
	hgrp, err := user.LookupGroupId(human.Gid)
	if err != nil {
		return 0, err
	}
	pgrp, err := user.LookupGroupId(persona.Gid)
	if err != nil {
		return 0, err
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
		return 0, fmt.Errorf("bindfs: %w", err)
	}
	exited := make(chan struct{})
	go func() { bindfs.Wait(); close(exited) }()
	defer func() { bindfs.Process.Signal(syscall.SIGTERM); <-exited }()
	if err := mounted(view, exited); err != nil {
		return 0, err
	}
	if err := syscall.Mount(view, home, "", syscall.MS_MOVE, ""); err != nil {
		return 0, err
	}
	defer syscall.Unmount(home, syscall.MNT_DETACH)
	if err := layer(home, pr); err != nil {
		return 0, err
	}

	os.Setenv("PATH", path)
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

// layer applies the profile's view of HOME: read-only with writable
// exceptions, and hidden paths shown empty. Paths are resolved inside HOME
// only; missing ones are skipped.
// ponytail: a mount on a single file falls off when the human replaces that
// file by rename; profiles should name directories.
func layer(home string, pr Persona) error {
	at := func(rel string) (string, os.FileInfo) {
		p, err := filepath.EvalSymlinks(filepath.Join(home, rel))
		if err != nil || !strings.HasPrefix(p, home+"/") {
			return "", nil
		}
		fi, err := os.Stat(p)
		if err != nil {
			return "", nil
		}
		return p, fi
	}
	const keep = syscall.MS_NOSUID | syscall.MS_NODEV
	rebind := func(p string, flags uintptr) error {
		if err := syscall.Mount(p, p, "", syscall.MS_BIND, ""); err != nil {
			return err
		}
		return syscall.Mount("", p, "", syscall.MS_BIND|syscall.MS_REMOUNT|keep|flags, "")
	}
	if pr.HomeReadOnly {
		if err := rebind(home, syscall.MS_RDONLY); err != nil {
			return fmt.Errorf("read-only HOME: %w", err)
		}
	}
	for _, rel := range pr.Writable {
		if p, fi := at(rel); fi != nil {
			if err := rebind(p, 0); err != nil {
				return fmt.Errorf("writable %s: %w", rel, err)
			}
		}
	}
	for _, rel := range pr.Hidden {
		p, fi := at(rel)
		var err error
		switch {
		case fi == nil:
		case fi.IsDir():
			err = syscall.Mount("tmpfs", p, "tmpfs", keep|syscall.MS_RDONLY|syscall.MS_NOEXEC, "size=4k,mode=0555")
		default:
			err = syscall.Mount("/dev/null", p, "", syscall.MS_BIND, "")
		}
		if err != nil {
			return fmt.Errorf("hidden %s: %w", rel, err)
		}
	}
	return nil
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
