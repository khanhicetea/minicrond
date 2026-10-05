package main

// Descriptor-relative, no-symlink filesystem helpers for `service install
// --user`. The installer runs as root but the path below the target user's
// home is controlled by that user, so every step below the home directory is
// an openat/mkdirat relative to an already-open directory descriptor with
// O_NOFOLLOW, and all ownership changes are fchown on descriptors we hold.
// A concurrently swapped-in symlink therefore cannot redirect a write or a
// chown outside the directory that was opened and validated.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// maxServiceConfigRead bounds how much of an existing user-owned config the
// root installer reads to learn its bind port.
const maxServiceConfigRead = 1 << 20

// serviceFSHook is a test-only seam invoked at named points of the install
// walk so tests can simulate a user replacing paths concurrently.
var serviceFSHook func(stage string)

func fsHook(stage string) {
	if serviceFSHook != nil {
		serviceFSHook(stage)
	}
}

func retryEINTR(f func() error) error {
	for {
		if err := f(); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

func openat(dirfd int, name string, flags int, mode uint32) (fd int, err error) {
	err = retryEINTR(func() (e error) {
		fd, e = unix.Openat(dirfd, name, flags|unix.O_CLOEXEC, mode)
		return e
	})
	return fd, err
}

func fstat(fd int) (st unix.Stat_t, err error) {
	err = retryEINTR(func() error { return unix.Fstat(fd, &st) })
	return st, err
}

// noFollowError turns the errno produced by O_NOFOLLOW on a symlink (ELOOP,
// or ENOTDIR together with O_DIRECTORY) into an explicit refusal.
func noFollowError(path string, err error) error {
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return fmt.Errorf("refusing %s: it is a symlink or not a directory/regular file", path)
	}
	return fmt.Errorf("open %s: %w", path, err)
}

// openHomeDir opens the user's home directory as the trust anchor. The path
// itself comes from the system user database, so it may legitimately be
// reached through administrator-made symlinks; everything below it may not.
// The home must belong to the target user or to root.
func openHomeDir(home string, uid int) (int, error) {
	fd, err := openat(unix.AT_FDCWD, home, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return -1, fmt.Errorf("open home directory %s: %w", home, err)
	}
	st, err := fstat(fd)
	if err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("stat home directory %s: %w", home, err)
	}
	if owner := int(st.Uid); owner != uid && owner != 0 {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("home directory %s is owned by uid %d, expected %d or root", home, owner, uid)
	}
	return fd, nil
}

// openUserDir opens (creating if missing) the directory name below parent
// without following symlinks. A directory that already exists must be owned
// by uid and is never chowned. A directory created here is handed to
// uid:gid through its descriptor.
func openUserDir(parent int, name, display string, uid, gid int) (int, error) {
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW
	created := false
	fd, err := openat(parent, name, flags, 0)
	if errors.Is(err, unix.ENOENT) {
		mkErr := retryEINTR(func() error { return unix.Mkdirat(parent, name, 0o700) })
		if mkErr != nil && !errors.Is(mkErr, unix.EEXIST) {
			return -1, fmt.Errorf("create directory %s: %w", display, mkErr)
		}
		created = mkErr == nil
		fd, err = openat(parent, name, flags, 0)
	}
	if err != nil {
		return -1, noFollowError(display, err)
	}
	st, err := fstat(fd)
	if err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("stat %s: %w", display, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("refusing %s: not a directory", display)
	}
	if !created {
		if int(st.Uid) != uid {
			_ = unix.Close(fd)
			return -1, fmt.Errorf("refusing %s: owned by uid %d, expected %d", display, st.Uid, uid)
		}
		return fd, nil
	}
	// We made this directory a moment ago; if the name was swapped for some
	// other directory in between, it will not be ours, 0700 and empty.
	if int(st.Uid) != os.Geteuid() || st.Mode&0o7077 != 0 {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("refusing %s: directory changed while it was being created", display)
	}
	if empty, err := dirIsEmpty(fd); err != nil || !empty {
		_ = unix.Close(fd)
		if err != nil {
			return -1, fmt.Errorf("inspect %s: %w", display, err)
		}
		return -1, fmt.Errorf("refusing %s: directory changed while it was being created", display)
	}
	if err := retryEINTR(func() error { return unix.Fchmod(fd, 0o700) }); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("chmod %s: %w", display, err)
	}
	if err := retryEINTR(func() error { return unix.Fchown(fd, uid, gid) }); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("chown %s: %w", display, err)
	}
	return fd, nil
}

func dirIsEmpty(fd int) (bool, error) {
	dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	f := os.NewFile(uintptr(dup), "dir")
	defer f.Close()
	names, err := f.Readdirnames(1)
	if err == io.EOF {
		return true, nil
	}
	return len(names) == 0, err
}

// installUserConfig creates the user's config at configPath (which must lie
// below home) with the given content, unless one already exists. It returns
// the existing config's bind port (0 if none or unreadable) and whether a new
// file was created. Nothing outside the opened directory chain is created,
// written, or chowned.
func installUserConfig(home, configPath string, uid, gid int, content string) (existingPort int, created bool, err error) {
	rel, err := filepath.Rel(home, configPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return 0, false, fmt.Errorf("config path %s is not inside home directory %s", configPath, home)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	dirfd, err := openHomeDir(home, uid)
	if err != nil {
		return 0, false, err
	}
	display := home
	for _, part := range parts[:len(parts)-1] {
		display = filepath.Join(display, part)
		next, err := openUserDir(dirfd, part, display, uid, gid)
		_ = unix.Close(dirfd)
		if err != nil {
			return 0, false, err
		}
		dirfd = next
	}
	defer unix.Close(dirfd)
	name := parts[len(parts)-1]

	fsHook("before-open-config")
	fd, err := openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	switch {
	case err == nil:
		port, err := readExistingConfigPort(fd, configPath, uid)
		return port, false, err
	case !errors.Is(err, unix.ENOENT):
		return 0, false, noFollowError(configPath, err)
	}

	fsHook("before-create-config")
	fd, err = openat(dirfd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return 0, false, fmt.Errorf("%s appeared while installing; retry", configPath)
		}
		return 0, false, fmt.Errorf("create %s: %w", configPath, err)
	}
	f := os.NewFile(uintptr(fd), configPath)
	fail := func(err error) (int, bool, error) {
		_ = f.Close()
		_ = unix.Unlinkat(dirfd, name, 0)
		return 0, false, err
	}
	if _, err := io.WriteString(f, content); err != nil {
		return fail(fmt.Errorf("write %s: %w", configPath, err))
	}
	if err := retryEINTR(func() error { return unix.Fchown(fd, uid, gid) }); err != nil {
		return fail(fmt.Errorf("chown %s: %w", configPath, err))
	}
	if err := f.Close(); err != nil {
		_ = unix.Unlinkat(dirfd, name, 0)
		return 0, false, fmt.Errorf("close %s: %w", configPath, err)
	}
	return 0, true, nil
}

// readExistingConfigPort validates an already-open existing config (regular
// file owned by uid) and returns its bind port. It takes ownership of fd.
func readExistingConfigPort(fd int, path string, uid int) (int, error) {
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := fstat(fd)
	if err != nil {
		return 0, fmt.Errorf("stat %s: %w", path, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return 0, fmt.Errorf("refusing %s: not a regular file", path)
	}
	if int(st.Uid) != uid {
		return 0, fmt.Errorf("refusing %s: owned by uid %d, expected %d", path, st.Uid, uid)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxServiceConfigRead))
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	return bindPortFromContent(b), nil
}

func bindPortFromContent(b []byte) int {
	m := bindPattern.FindSubmatch(b)
	if m == nil {
		return 0
	}
	_, port, err := net.SplitHostPort(string(m[1]))
	if err != nil {
		return 0
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return 0
	}
	return p
}
