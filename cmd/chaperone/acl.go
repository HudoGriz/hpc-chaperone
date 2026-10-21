package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// ACL entries name users by UID, so cleanup works after a persona is gone.

// shareHome shares the profile's HOME-relative paths with the persona (UID
// pid): read-write (write) or read-only. A missing write directory is created
// as the human; a missing read path is skipped (tool not installed). Files
// are granted on every launch: an atomic replace drops their ACL. Symlinked
// components are refused. Runs as the human.
func shareHome(human *user.User, pid string, paths []string, write bool) error {
next:
	for _, rel := range paths {
		dir := human.HomeDir
		parts := strings.Split(rel, "/")
		for i, part := range parts {
			dir = filepath.Join(dir, part)
			info, err := os.Lstat(dir)
			if os.IsNotExist(err) {
				if !write {
					continue next
				}
				if err := os.Mkdir(dir, 0700); err != nil {
					return err
				}
				info, err = os.Lstat(dir)
			}
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing symlink in shared path %s", dir)
			}
			if i < len(parts)-1 {
				if err := grantPath(dir, "u:"+pid+":--x"); err != nil {
					return err
				}
				continue
			}
			switch {
			case info.IsDir():
				err = shareDir(dir, human, pid, write, write)
			case info.Mode().IsRegular():
				err = grantPath(dir, "u:"+pid+":"+fileClass(info.Mode(), write))
			default:
				err = fmt.Errorf("shared path is neither file nor directory: %s", dir)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// fileClass is the owner's access class for the persona: rw-/rwx when
// written, r--/r-x when read (an executable stays executable).
func fileClass(mode os.FileMode, write bool) string {
	perm := "r-"
	if write {
		perm = "rw"
	}
	if mode&0100 != 0 {
		return perm + "x"
	}
	return perm + "-"
}

// workdir accepts the launch directory for sharing: a directory the human
// owns, not HOME or above it. Root records it before the human shares it, so
// both apply exactly these rules.
func workdir(human *user.User, wd string) (string, error) {
	dir, err := filepath.EvalSymlinks(wd)
	if err != nil {
		return "", err
	}
	home, err := filepath.EvalSymlinks(human.HomeDir)
	if err != nil {
		return "", err
	}
	if inside(home, dir) {
		return "", fmt.Errorf("%s is not shared with the persona; run from a project directory", dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if owner(info) != human.Uid {
		return "", fmt.Errorf("%s is not yours; not shared with the persona", dir)
	}
	return dir, nil
}

// shareWorkdir lets the persona work where it was launched. Only the first
// launch in a tree walks it; later files inherit the default ACL.
// ponytail: a file a tool creates 0600 there stays masked from the other user
// until `chaperone -- chmod`; add a bounded repair pass if that bites.
func shareWorkdir(human *user.User, pid, wd string) error {
	dir, err := workdir(human, wd)
	if err != nil {
		return err
	}
	home, _ := filepath.EvalSymlinks(human.HomeDir)
	if inside(dir, home) {
		for p := filepath.Dir(dir); p != home; p = filepath.Dir(p) {
			if err := grantPath(p, "u:"+pid+":--x"); err != nil {
				return err
			}
		}
	}
	return shareDir(dir, human, pid, false, true)
}

// shareDir gives both users rwx (write) or the persona r-x on dir and, by
// default ACL, on everything created below it. The first share also walks
// the human's existing files (within walkBudget); repair re-walks for masked
// entries on every launch (small agent dirs only). dir is pinned by
// descriptor and must still be the human's directory at the expected path.
// On first share the walk runs before dir itself admits the persona.
func shareDir(path string, human *user.User, pid string, repair, write bool) error {
	owner, err := strconv.Atoi(human.Uid)
	if err != nil {
		return err
	}
	dir, st, err := openAt(atFDCWD, path)
	if err != nil {
		return err
	}
	defer dir.Close()
	want, _ := filepath.EvalSymlinks(path)
	got, _ := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", dir.Fd()))
	if st.Mode&syscall.S_IFMT != syscall.S_IFDIR || int(st.Uid) != owner || got != want {
		return fmt.Errorf("%s is not a directory owned by %s", path, human.Username)
	}
	perm, ids := "r-x", []string{pid}
	if write {
		perm, ids = "rwx", []string{pid, human.Uid}
	}
	out, _ := fdACL("getfacl", []string{"-cpn", "--"}, dir)
	full := !bytes.Contains(out, []byte("default:user:"+pid+":"+perm))
	if full {
		if err := repairACL(dir, owner, true, write, ids...); err != nil {
			return err
		}
	}
	var top []string
	for _, u := range ids {
		top = append(top, "u:"+u+":"+perm, "d:u:"+u+":"+perm)
	}
	// Fatal here: an unsupported filesystem must stop the launch.
	if err := grantFD(dir, top...); err != nil {
		return err
	}
	if !full && repair {
		if err := repairACL(dir, owner, false, write, ids...); err != nil {
			fmt.Fprintln(os.Stderr, "chaperone: warning: shared-state ACL repair incomplete:", err)
		}
	}
	return nil
}

// grantPath adds ACL entries to one file or directory without following a
// final symlink; see grantFD.
func grantPath(path string, entries ...string) error {
	f, _, err := openAt(atFDCWD, path)
	if err != nil {
		return err
	}
	defer f.Close()
	return grantFD(f, entries...)
}

// grantFD adds entries to f's ACL without widening anyone else: existing
// named entries and the owning group are first cut to what the current mask
// lets them have, then setfacl recalculates the mask. Keeping the mask
// instead (setfacl -n) would leave a new entry masked out on a 0700
// directory. Known ceiling: bits an existing mask hid are dropped from the
// stored entries, so revoking restores effective access, not that text.
func grantFD(f *os.File, entries ...string) error {
	keep, err := effective(f, nil)
	if err != nil {
		return err
	}
	_, err = fdACL("setfacl", []string{"-m", strings.Join(append(keep, entries...), ","), "--"}, f)
	return err
}

// revokeFD removes the access entries of uids from f, cutting the others to
// their effective rights first so the recalculated mask widens no one.
func revokeFD(f *os.File, uids ...string) error {
	keep, err := effective(f, uids)
	if err != nil {
		return err
	}
	var gone []string
	for _, u := range uids {
		gone = append(gone, "u:"+u)
	}
	args := []string{"-x", strings.Join(gone, ",")}
	if len(keep) > 0 {
		args = append(args, "-m", strings.Join(keep, ","))
	}
	_, err = fdACL("setfacl", append(args, "--"), f)
	return err
}

// effective lists f's named access entries (except uids) and its owning
// group, each cut to the mask; nil when there is no mask to cut to.
func effective(f *os.File, skip []string) ([]string, error) {
	out, err := fdACL("getfacl", []string{"-cpEn", "--"}, f)
	if err != nil {
		return nil, err
	}
	var mask string
	lines := strings.Split(string(out), "\n")
	for _, l := range lines {
		if m, ok := strings.CutPrefix(l, "mask::"); ok {
			mask = m
		}
	}
	if len(mask) != 3 {
		return nil, nil
	}
	var keep []string
	for _, l := range lines {
		e := strings.Split(l, ":") // tag:id:perm; the owner (user::) is never masked
		if len(e) != 3 || len(e[2]) != 3 || (e[0] != "user" && e[0] != "group") || (e[0] == "user" && e[1] == "") {
			continue
		}
		if e[0] == "user" && slices.Contains(skip, e[1]) {
			continue
		}
		perm := []byte(e[2])
		for i := range perm {
			if mask[i] == '-' {
				perm[i] = '-'
			}
		}
		keep = append(keep, e[0][:1]+":"+e[1]+":"+string(perm))
	}
	return keep, nil
}

// Linux constants absent from package syscall. O_PATH pins an inode without
// opening it for I/O.
const (
	oPath   = 0x200000
	atFDCWD = -0x64
)

// openAt opens name relative to dirfd without following a final symlink:
// a directory for reading, anything else as an O_PATH handle.
func openAt(dirfd int, name string) (*os.File, syscall.Stat_t, error) {
	var st syscall.Stat_t
	fd, err := syscall.Openat(dirfd, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err == syscall.ENOTDIR {
		fd, err = syscall.Openat(dirfd, name, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, st, err
	}
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return nil, st, err
	}
	return os.NewFile(uintptr(fd), name), st, nil
}

// fdACL runs getfacl/setfacl on pinned files, each named by the child's
// /proc/self/fd magic link: a pathname swapped for a symlink after the check
// cannot redirect it.
func fdACL(tool string, args []string, files ...*os.File) ([]byte, error) {
	for i := range files {
		args = append(args, fmt.Sprintf("/proc/self/fd/%d", 3+i))
	}
	cmd := exec.Command(tool, args...)
	cmd.ExtraFiles = files
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s: %v: %s", tool, err, bytes.TrimSpace(out))
	}
	return out, nil
}

// walkBudget caps the entries one first share may touch: every ACL write is
// a metadata operation on a shared filesystem. _share sets it from the
// policy's walk_limit.
var walkBudget int

var errTooBig = errors.New("too many files to share; launch from a smaller directory")

// repairACL gives ids the owner's access class on what owner owns below
// root: rwx/rwx/rw for directories, executables and other files (write), or
// r-x/r-x/r-- (read-only), plus default entries on directories. It runs as
// owner, never as root, and every object is opened without following
// symlinks and changed through its descriptor. full=false only touches
// entries whose ACL mask a 0600/0700 create cut off (mode group bits show
// the mask), so routine launches stat but rarely write. A full pass stops at
// walkBudget entries.
// Known ceilings: the mask recalculation re-enables other masked named
// entries inside shared trees; files moved in with group bits and no
// inherited ACL are skipped until a full pass; multiply linked files are
// skipped (an ACL would reach every alias).
// ponytail: full walk per launch of the agent dirs; add a ctime stamp if MDS load shows it.
func repairACL(root *os.File, owner int, full, write bool, ids ...string) error {
	acl := func(perm string, dflt bool) string {
		var e []string
		for _, u := range ids {
			e = append(e, "u:"+u+":"+perm)
			if dflt {
				e = append(e, "d:u:"+u+":"+perm)
			}
		}
		return strings.Join(e, ",")
	}
	// Name the permission exactly: setfacl's X would make data files executable.
	type batch struct {
		acl   string
		need  uint32 // mode group bits (the mask) when already shared
		files []*os.File
	}
	b := []batch{{acl("rwx", true), 0070, nil}, {acl("rwx", false), 0070, nil}, {acl("rw-", false), 0060, nil}}
	if !write {
		b = []batch{{acl("r-x", true), 0050, nil}, {acl("r-x", false), 0050, nil}, {acl("r--", false), 0040, nil}}
	}
	var errs []error
	seen := 0
	flush := func(i int) {
		if len(b[i].files) > 0 {
			if _, err := fdACL("setfacl", []string{"-m", b[i].acl, "--"}, b[i].files...); err != nil {
				errs = append(errs, err)
			}
		}
		for _, f := range b[i].files {
			f.Close()
		}
		b[i].files = nil
	}
	var walk func(d *os.File) bool
	walk = func(d *os.File) bool {
		for {
			names, err := d.Readdirnames(256)
			if len(names) == 0 {
				if err != nil && !errors.Is(err, io.EOF) {
					errs = append(errs, err)
				}
				return true
			}
			for _, n := range names {
				if seen++; full && seen > walkBudget {
					return false
				}
				f, st, err := openAt(int(d.Fd()), n)
				if err != nil {
					continue // vanished, unreadable, or a symlink (ELOOP)
				}
				mode := st.Mode & 0777
				i := -1
				switch st.Mode & syscall.S_IFMT {
				case syscall.S_IFDIR:
					if !walk(f) {
						f.Close()
						return false
					}
					i = 0
				case syscall.S_IFREG:
					if st.Nlink == 1 && mode&0100 != 0 {
						i = 1
					} else if st.Nlink == 1 {
						i = 2
					}
				}
				if i < 0 || int(st.Uid) != owner || (!full && mode&b[i].need == b[i].need) {
					f.Close()
					continue
				}
				if b[i].files = append(b[i].files, f); len(b[i].files) == 128 {
					flush(i)
				}
			}
		}
	}
	complete := walk(root)
	for i := range b {
		flush(i)
	}
	if !complete {
		errs = append(errs, errTooBig)
	}
	return errors.Join(errs...)
}

func owner(fi os.FileInfo) string {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return strconv.FormatUint(uint64(st.Uid), 10)
	}
	return ""
}
