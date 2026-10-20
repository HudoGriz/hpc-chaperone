package main

import (
	"encoding/binary"
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

// grantsFile is root's record of everything launches shared, written before
// the sharing happens. Uninstall and policy revocation read it, so neither
// depends on the current policy or on NSS.
var grantsFile = "/var/lib/chaperone/grants"

// grant is one shared root: HOME itself (kind h), a profile path (p) or a
// launch directory (w). IDs are numeric: ACL entries use them.
type grant struct {
	persona, puid, human, huid, hgid, home, kind, path string
}

func (g grant) line() string {
	return strings.Join([]string{g.persona, g.puid, g.human, g.huid, g.hgid, strconv.Quote(g.home), g.kind, strconv.Quote(g.path)}, "\t")
}

func readGrants() ([]grant, error) {
	data, err := os.ReadFile(grantsFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var gs []grant
	for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if l == "" {
			continue
		}
		f := strings.Split(l, "\t")
		if len(f) != 8 {
			return nil, fmt.Errorf("corrupt grant record: invalid field count")
		}
		home, err1 := strconv.Unquote(f[5])
		path, err2 := strconv.Unquote(f[7])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("corrupt grant record: %q", l)
		}
		gs = append(gs, grant{f[0], f[1], f[2], f[3], f[4], home, f[6], path})
	}
	return gs, nil
}

func writeGrants(gs []grant) error {
	var b strings.Builder
	for _, g := range gs {
		b.WriteString(g.line() + "\n")
	}
	tmp := grantsFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, grantsFile)
}

// lockGrants serializes record, revocation and uninstall on this host.
func lockGrants() (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(grantsFile), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(grantsFile+".lock", os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// record writes what this launch will share before it is shared, and
// revokes profile paths the policy no longer grants this persona: a policy
// change applies on the next launch. Runs as root.
func record(human, persona *user.User, pr Persona, wd string) error {
	home, err := filepath.EvalSymlinks(human.HomeDir)
	if err != nil {
		return err
	}
	base := grant{persona: persona.Username, puid: persona.Uid, human: human.Username, huid: human.Uid, hgid: human.Gid, home: home}
	at := func(kind, path string) grant { g := base; g.kind, g.path = kind, path; return g }
	want := []grant{at("h", home)}
	for _, rel := range append(slices.Clone(pr.Write), pr.Read...) {
		want = append(want, at("p", filepath.Join(home, rel)))
	}
	if pr.Workdir != "" && pr.Workdir != "off" {
		dir, err := workdir(human, wd)
		if err == nil {
			want = append(want, at("w", dir))
		} else if pr.Workdir != "auto" || !errors.Is(err, errWorkdirNotEligible) {
			return fmt.Errorf("working directory: %w", err)
		}
	}
	lock, err := lockGrants()
	if err != nil {
		return err
	}
	defer lock.Close()
	old, err := readGrants()
	if err != nil {
		return err
	}
	var keep []grant
	for _, g := range old {
		if g.persona == persona.Username && g.kind == "p" && !slices.Contains(want, g) {
			if err := clean(g); err != nil {
				return fmt.Errorf("revoking %s: %w", g.path, err)
			}
			continue
		}
		keep = append(keep, g)
	}
	for _, g := range want {
		if !slices.Contains(keep, g) {
			keep = append(keep, g)
		}
	}
	return writeGrants(keep)
}

// runUninstall stops every recorded persona's processes on this host. To
// purge, it also undoes what launches shared, from the record: persona-owned
// files go back to their humans and both users' ACL entries are removed.
// Retiring keeps the files and (now inert) entries: the persona stays in the
// directory, disabled, so nothing is orphaned and no tree is walked. Run it
// as root after removing the sudo rule and cancelling persona jobs.
func runUninstall(purge bool) error {
	if os.Geteuid() != 0 {
		return errors.New("_uninstall must run as root")
	}
	os.Setenv("PATH", safePath)
	lock, err := lockGrants()
	if err != nil {
		return err
	}
	defer lock.Close()
	gs, err := readGrants()
	if err != nil {
		return err
	}
	var errs []error
	killed := map[string]bool{}
	var names []string
	for _, g := range gs {
		if !killed[g.puid] {
			names = append(names, g.persona)
			killed[g.puid] = true
			exec.Command("pkill", "-KILL", "-u", g.puid).Run() // exit 1: none running
		}
	}
	if !purge {
		for _, name := range names {
			fmt.Println(name)
		}
		return nil // the record stays: a later purge needs it
	}
	for _, g := range gs {
		if err := clean(g); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", g.path, err))
		}
	}
	if len(errs) == 0 {
		if err := os.Remove(grantsFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for _, name := range names {
			fmt.Println(name)
		}
	}
	return errors.Join(errs...)
}

// clean undoes one grant as root: the tree for p/w, and the traverse
// entries on its parents up to HOME; HOME itself for h.
func clean(g grant) error {
	if g.kind == "h" {
		return revokePath(g.path, g.puid)
	}
	if err := cleanTree(g); err != nil {
		return err
	}
	for d := filepath.Dir(g.path); inside(d, g.home) && d != g.home; d = filepath.Dir(d) {
		if err := revokePath(d, g.puid); err != nil {
			return err
		}
	}
	return nil
}

func revokePath(path, uid string) error {
	f, _, err := openAbs(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	if err := revokeFD(f, uid); err != nil {
		return err
	}
	if bare(fmt.Sprintf("/proc/self/fd/%d", f.Fd())) {
		_, err = fdACL("setfacl", []string{"-b", "--"}, f)
	}
	return err
}

// openAbs opens an absolute path one component at a time, never following
// a symlink: as root, a path the human can rename must not lead elsewhere.
func openAbs(path string) (*os.File, syscall.Stat_t, error) {
	var st syscall.Stat_t
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return nil, st, fmt.Errorf("not a clean absolute path: %q", path)
	}
	fd, err := syscall.Open("/", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, st, err
	}
	parts := strings.Split(path[1:], "/")
	for _, p := range parts[:len(parts)-1] {
		next, err := syscall.Openat(fd, p, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(fd)
		if err != nil {
			return nil, st, &os.PathError{Op: "open", Path: path, Err: err}
		}
		fd = next
	}
	defer syscall.Close(fd)
	f, st, err := openAt(fd, parts[len(parts)-1])
	if err != nil {
		return nil, st, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return f, st, nil
}

const atEmptyPath = 0x1000

// cleanTree hands persona-owned objects in a shared tree back to the human
// and strips both users' named entries. Every object is pinned by
// descriptor before its owner is checked and changed, symlinks are never
// followed (their own owner is fixed), and the walk stays on one filesystem.
func cleanTree(g grant) error {
	root, st, err := openAbs(g.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	huid, _ := strconv.Atoi(g.huid)
	hgid, _ := strconv.Atoi(g.hgid)
	var errs []error
	var dirs, files []*os.File
	strip := func(force bool) {
		for _, b := range []struct {
			fs  *[]*os.File
			acl string
		}{{&dirs, "u:" + g.puid + ",d:u:" + g.puid + ",u:" + g.huid + ",d:u:" + g.huid}, {&files, "u:" + g.puid + ",u:" + g.huid}} {
			if len(*b.fs) == 0 || (!force && len(*b.fs) < 128) {
				continue
			}
			if _, err := fdACL("setfacl", []string{"-x", b.acl, "--"}, *b.fs...); err != nil {
				errs = append(errs, err)
			}
			// What -x leaves (a mask, default base entries) still shows as
			// an ACL: drop ACLs with no named entry left.
			var empty []*os.File
			for _, f := range *b.fs {
				if bare(fmt.Sprintf("/proc/self/fd/%d", f.Fd())) {
					empty = append(empty, f)
				}
			}
			if len(empty) > 0 {
				if _, err := fdACL("setfacl", []string{"-b", "--"}, empty...); err != nil {
					errs = append(errs, err)
				}
			}
			for _, f := range *b.fs {
				f.Close()
			}
			*b.fs = nil
		}
	}
	// visit takes ownership of f.
	var visit func(f *os.File, st syscall.Stat_t)
	visit = func(f *os.File, st syscall.Stat_t) {
		if strconv.Itoa(int(st.Uid)) == g.puid {
			if err := syscall.Fchownat(int(f.Fd()), "", huid, hgid, atEmptyPath); err != nil {
				errs = append(errs, err)
			}
		}
		switch st.Mode & syscall.S_IFMT {
		case syscall.S_IFDIR:
			children(f, st.Dev, func(c *os.File, cst syscall.Stat_t) { visit(c, cst) }, &errs)
			dirs = append(dirs, f)
		case syscall.S_IFREG:
			files = append(files, f)
		default:
			f.Close()
		}
		strip(false)
	}
	visit(root, st)
	strip(true)
	return errors.Join(errs...)
}

// children pins each entry of dir, symlinks included (O_PATH|O_NOFOLLOW
// opens the link itself), on device dev only.
func children(dir *os.File, dev uint64, fn func(*os.File, syscall.Stat_t), errs *[]error) {
	for {
		names, err := dir.Readdirnames(256)
		for _, n := range names {
			fd, err := syscall.Openat(int(dir.Fd()), n, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			if err != nil {
				continue // vanished
			}
			var st syscall.Stat_t
			if syscall.Fstat(fd, &st) != nil || st.Dev != dev {
				syscall.Close(fd)
				continue
			}
			if st.Mode&syscall.S_IFMT == syscall.S_IFDIR { // reopen for listing
				d, err := syscall.Openat(fd, ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
				syscall.Close(fd)
				if err != nil {
					continue
				}
				fd = d
			}
			fn(os.NewFile(uintptr(fd), n), st)
		}
		if len(names) == 0 {
			if err != nil && !errors.Is(err, io.EOF) {
				*errs = append(*errs, err)
			}
			return
		}
	}
}

// bare reports whether path has an access or default ACL without a named
// user or group entry. Format: 4-byte version, then 8-byte entries
// {tag u16, perm u16, id u32}; tags 2 and 8 are named user and group.
func bare(path string) bool {
	found := false
	for _, name := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		buf := make([]byte, 4096)
		n, err := syscall.Getxattr(path, name, buf)
		if err != nil {
			continue
		}
		found = true
		for i := 4; i+8 <= n; i += 8 {
			if tag := binary.LittleEndian.Uint16(buf[i:]); tag == 2 || tag == 8 {
				return false
			}
		}
	}
	return found
}
