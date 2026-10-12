// Package trustedpath validates opt-in executable search paths. Trusted means
// administrator-controlled: root owns every traversed directory and file, with
// no group/other write permission. Root is already the trusted administrator,
// not a sandboxed job identity. This deliberately rejects user-owned toolchains
// (even chmod 0555: their owner could chmod them back).
package trustedpath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Validate accepts at most 32 absolute directories. Nil retains legacy lookup;
// an explicitly empty list is an error rather than silently changing policy.
func Validate(dirs []string) error {
	if dirs == nil {
		return nil
	}
	if len(dirs) == 0 || len(dirs) > 32 {
		return errors.New("must contain between 1 and 32 directories")
	}
	for _, dir := range dirs {
		_, info, err := resolve(dir)
		if err != nil {
			return fmt.Errorf("directory %q: %w", dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%q is not a directory", dir)
		}
	}
	return nil
}

// LookPath never consults ambient or definition-provided PATH. Callers validate
// the directory list at spawn time; candidate files are checked here too.
func LookPath(program string, dirs []string) (string, error) {
	if program == "" || strings.ContainsRune(program, '/') {
		return "", errors.New("expected a bare executable name")
	}
	for _, dir := range dirs {
		path, info, err := resolve(strings.TrimRight(dir, "/") + "/" + program)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("trusted executable %q: %w", program, err)
		}
		if info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return path, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in trusted PATH", program)
}

// resolve walks symlinks component by component, checking *all* traversed
// directories, not merely EvalSymlinks' final canonical path. A link via a
// writable intermediate directory must not become trusted just because it
// currently resolves back into /usr/bin. At most 40 symlinks are followed.
func resolve(path string) (string, fs.FileInfo, error) {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, ':') || len(path) > 4096 {
		return "", nil, errors.New("must be an absolute path of at most 4096 bytes without ':'")
	}
	current := string(filepath.Separator)
	info, err := os.Lstat(current)
	if err != nil {
		return "", nil, err
	}
	if err := trusted(info); err != nil {
		return "", nil, fmt.Errorf("%s: %w", current, err)
	}
	parts := strings.Split(path, "/")
	links := 0
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			current = filepath.Dir(current)
			continue
		}
		candidate := filepath.Join(current, part)
		info, err = os.Lstat(candidate)
		if err != nil {
			return "", nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > 40 {
				return "", nil, errors.New("too many symlinks")
			}
			target, err := os.Readlink(candidate)
			if err != nil {
				return "", nil, err
			}
			if filepath.IsAbs(target) {
				current = "/"
			}
			parts = append(strings.Split(target, "/"), parts...)
			continue
		}
		if len(parts) > 0 && !info.IsDir() {
			return "", nil, fmt.Errorf("%q is not a directory", candidate)
		}
		if err := trusted(info); err != nil {
			return "", nil, fmt.Errorf("%s: %w", candidate, err)
		}
		current = candidate
	}
	info, err = os.Lstat(current)
	if err != nil {
		return "", nil, err
	}
	return current, info, nil
}

func trusted(info fs.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("file ownership is unavailable")
	}
	// The group bits include the POSIX ACL mask, so an ACL granting write to a
	// named non-root user/group also fails this check on supported Linux systems.
	if stat.Uid != 0 || info.Mode().Perm()&0022 != 0 {
		return errors.New("must be root-owned and not group/other writable")
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return errors.New("must be a regular file or directory")
	}
	return nil
}
