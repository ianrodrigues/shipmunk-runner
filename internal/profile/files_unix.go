//go:build darwin || linux

package profile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

var profileIdentifierPattern = regexp.MustCompile(`^[0-9a-hjkmnp-tv-z]{26}$`)

func prepareProfileRoot(path string) error {
	if err := rejectSymlinkComponents(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, 0700); err != nil {
			return err
		}
		if err := os.Chmod(path, 0700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	return validateProfileDirectory(info, 0700)
}

func ensureProfileDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, 0700); err != nil {
			return err
		}
		if err := os.Chmod(path, 0700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	return validateProfileDirectory(info, 0700)
}

func rejectSymlinkComponents(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("profile path is not canonical")
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, current), current) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("symlinks are forbidden in the profile root")
		}
		if current != path && !info.IsDir() {
			return errors.New("profile root ancestor is not a directory")
		}
	}
	return nil
}

func validateProfileDirectory(info os.FileInfo, mode os.FileMode) error {
	if info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Mode().Perm() != mode {
		return errors.New("profile directory has unsafe type or permissions")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("profile directory is not owned by the current user")
	}
	return nil
}

func validateProfileFile(info os.FileInfo) error {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Mode().Perm() != 0600 {
		return errors.New("profile file has unsafe type or permissions")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return errors.New("profile file has unsafe ownership or link count")
	}
	return nil
}

func (store *Store) verifyDirectories() error {
	if err := rejectSymlinkComponents(store.root); err != nil {
		return fmt.Errorf("unsafe profile root: %w", err)
	}
	rootInfo, err := os.Lstat(store.root)
	if err != nil {
		return fmt.Errorf("inspect profile root: %w", err)
	}
	if err := validateProfileDirectory(rootInfo, 0700); err != nil {
		return err
	}
	profileInfo, err := os.Lstat(store.directory)
	if err != nil {
		return fmt.Errorf("inspect profile directory: %w", err)
	}
	return validateProfileDirectory(profileInfo, 0700)
}

func profilePathStat(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	return info, nil
}

func validateProfileLeaf(path string) error {
	info, err := profilePathStat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return validateProfileFile(info)
}

func openProfileLock(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_EXCL|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	created := err == nil
	if errors.Is(err, syscall.EEXIST) {
		fd, err = syscall.Open(path, syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	opened, statErr := file.Stat()
	current, lstatErr := os.Lstat(path)
	if statErr != nil || lstatErr != nil || !os.SameFile(opened, current) {
		_ = file.Close()
		return nil, errors.New("profile lock was replaced")
	}
	if created {
		if err := file.Chmod(0600); err != nil {
			_ = file.Close()
			return nil, err
		}
		opened, statErr = file.Stat()
		if statErr != nil {
			_ = file.Close()
			return nil, statErr
		}
	}
	if err := validateProfileFile(opened); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	opened, statErr = file.Stat()
	current, lstatErr = os.Lstat(path)
	if statErr != nil || lstatErr != nil || !os.SameFile(opened, current) {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = file.Close()
		return nil, errors.New("profile lock was replaced")
	}
	return file, nil
}

func unlockProfileLock(file *os.File) error {
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

func openProfileRead(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	opened, statErr := file.Stat()
	current, lstatErr := os.Lstat(path)
	if statErr != nil || lstatErr != nil || !os.SameFile(opened, current) {
		_ = file.Close()
		return nil, errors.New("profile journal changed during open")
	}
	if err := validateProfileFile(opened); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func createProfileFile(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return file, nil
}

func writeProfileFile(file *os.File, contents []byte) error {
	for len(contents) > 0 {
		written, err := file.Write(contents)
		if err != nil {
			return err
		}
		if written == 0 {
			return errors.New("short profile journal write")
		}
		contents = contents[written:]
	}
	return file.Sync()
}

func syncProfileDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

type nativeProfileEntry struct {
	root      *os.Root
	path      string
	info      os.FileInfo
	directory bool
	dev       uint64
	ino       uint64
	mode      uint32
	uid       uint32
	nlink     uint64
}

type nativeTreeHooks struct {
	beforeDirectoryOpen func(parent *os.Root, name string)
	beforeEntryChmod    func(entry nativeProfileEntry)
}

func inspectNativeProfileTree(home string, hooks nativeTreeHooks) ([]nativeProfileEntry, *os.Root, error) {
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, nil, err
	}
	info, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	pathInfo, err := os.Lstat(home)
	if err != nil || !os.SameFile(info, pathInfo) {
		_ = root.Close()
		return nil, nil, errors.New("native home changed during inspection")
	}
	if err := validateProfileDirectory(info, 0700); err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	entries := []nativeProfileEntry{}
	if err := inspectNativeProfileEntries(root, ".", &entries, hooks); err != nil {
		_ = root.Close()
		return nil, nil, err
	}
	return entries, root, nil
}

func inspectNativeProfileEntries(root *os.Root, directory string, entries *[]nativeProfileEntry, hooks nativeTreeHooks) error {
	children, err := fs.ReadDir(root.FS(), directory)
	if err != nil {
		return err
	}
	for _, child := range children {
		name := child.Name()
		entryPath := filepath.Join(directory, name)
		info, err := root.Lstat(entryPath)
		if err != nil {
			return err
		}
		directory := info.IsDir() && info.Mode()&os.ModeSymlink == 0
		if (!directory && !info.Mode().IsRegular()) || info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSetuid != 0 || info.Mode()&os.ModeSetgid != 0 || info.Mode()&os.ModeSticky != 0 {
			return errors.New("unsafe native-generated profile entry")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != uint32(os.Geteuid()) || (!directory && stat.Nlink != 1) {
			return errors.New("unsafe native-generated profile ownership or link count")
		}
		entry := nativeProfileEntry{
			root:      root,
			path:      entryPath,
			info:      info,
			directory: directory,
			dev:       uint64(stat.Dev),
			ino:       uint64(stat.Ino),
			mode:      uint32(stat.Mode),
			uid:       uint32(stat.Uid),
			nlink:     uint64(stat.Nlink),
		}
		*entries = append(*entries, entry)
		if directory {
			if hooks.beforeDirectoryOpen != nil {
				hooks.beforeDirectoryOpen(root, entryPath)
			}
			childRoot, err := root.OpenRoot(entryPath)
			if err != nil {
				return err
			}
			openedInfo, err := childRoot.Stat(".")
			if err != nil || !os.SameFile(info, openedInfo) {
				_ = childRoot.Close()
				return errors.New("native home changed during inspection")
			}
			_ = childRoot.Close()
			if err := inspectNativeProfileEntries(root, entryPath, entries, hooks); err != nil {
				return err
			}
		}
	}
	return nil
}

func verifyNativeSnapshot(entry nativeProfileEntry) error {
	info, err := entry.root.Lstat(entry.path)
	if err != nil {
		return errors.New("native home changed during normalization")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Dev) != entry.dev || uint64(stat.Ino) != entry.ino || uint32(stat.Mode) != entry.mode || uint32(stat.Uid) != entry.uid || uint64(stat.Nlink) != entry.nlink {
		return errors.New("native home changed during normalization")
	}
	return nil
}

func chmodNativeSnapshot(entry nativeProfileEntry) error {
	if err := verifyNativeSnapshot(entry); err != nil {
		return err
	}
	flags := os.O_RDONLY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC
	if entry.directory {
		flags |= syscall.O_DIRECTORY
	}
	file, err := entry.root.OpenFile(entry.path, flags, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameNativeSnapshot(entry, opened) {
		return errors.New("native home changed during normalization")
	}
	mode := os.FileMode(0600)
	if entry.directory {
		mode = 0700
	}
	return file.Chmod(mode)
}

func sameNativeSnapshot(entry nativeProfileEntry, info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Dev) == entry.dev && uint64(stat.Ino) == entry.ino && uint32(stat.Mode) == entry.mode && uint32(stat.Uid) == entry.uid && uint64(stat.Nlink) == entry.nlink
}

func normalizeNativeProfileTree(home string, hooks nativeTreeHooks) error {
	entries, root, err := inspectNativeProfileTree(home, hooks)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, entry := range entries {
		if hooks.beforeEntryChmod != nil {
			hooks.beforeEntryChmod(entry)
		}
		if err := chmodNativeSnapshot(entry); err != nil {
			return fmt.Errorf("protect native profile entry: %w", err)
		}
	}
	return validateNativeProfileTree(home)
}

func validateNativeProfileTree(home string) error {
	entries, root, err := inspectNativeProfileTree(home, nativeTreeHooks{})
	if err != nil {
		return err
	}
	defer root.Close()
	for _, entry := range entries {
		mode := os.FileMode(0600)
		if entry.directory {
			mode = 0700
		}
		if entry.info.Mode().Perm() != mode {
			return errors.New("native home has unsafe permissions")
		}
	}
	return nil
}

func validateProfileTree(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := validateProfileDirectory(info, 0700); err != nil {
		return err
	}
	children, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, child := range children {
		childPath := filepath.Join(path, child.Name())
		childInfo, err := os.Lstat(childPath)
		if err != nil {
			return err
		}
		if childInfo.IsDir() && childInfo.Mode()&os.ModeSymlink == 0 {
			if err := validateProfileTree(childPath); err != nil {
				return err
			}
		} else if err := validateProfileFile(childInfo); err != nil {
			return err
		}
	}
	return nil
}

func removeProfileTree(path string, hooks nativeTreeHooks) error {
	root, err := os.OpenRoot(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	rootInfo, err := root.Stat(".")
	if err != nil {
		return err
	}
	pathInfo, err := os.Lstat(path)
	if err != nil || !os.SameFile(rootInfo, pathInfo) || !rootInfo.IsDir() {
		return errors.New("native home changed during invalidation")
	}
	if err := removeProfileEntries(root, ".", hooks); err != nil {
		return err
	}
	if err := root.Close(); err != nil {
		return err
	}
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	name := filepath.Base(path)
	current, err := parent.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if os.SameFile(rootInfo, current) {
		return parent.Remove(name)
	}
	// If the home entry was replaced, remove only that entry. Root.Remove does
	// not follow a symlink, so an outside target remains untouched.
	return parent.Remove(name)
}

func removeProfileEntries(root *os.Root, directory string, hooks nativeTreeHooks) error {
	children, err := fs.ReadDir(root.FS(), directory)
	if err != nil {
		return err
	}
	for _, child := range children {
		name := child.Name()
		entryPath := filepath.Join(directory, name)
		info, err := root.Lstat(entryPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			if hooks.beforeDirectoryOpen != nil {
				hooks.beforeDirectoryOpen(root, entryPath)
			}
			childRoot, openErr := root.OpenRoot(entryPath)
			if openErr != nil {
				// Removing a replaced symlink is safe: Root.Remove never follows it.
				if removeErr := root.Remove(entryPath); removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
					continue
				}
				return openErr
			}
			openedInfo, statErr := childRoot.Stat(".")
			_ = childRoot.Close()
			if statErr != nil || !os.SameFile(info, openedInfo) {
				// The name now denotes a replacement. Remove only the entry itself;
				// never recurse through a path that was not the inspected directory.
				if removeErr := root.Remove(entryPath); removeErr == nil || errors.Is(removeErr, os.ErrNotExist) {
					continue
				}
				return errors.New("native home changed during invalidation")
			}
			recurseErr := removeProfileEntries(root, entryPath, hooks)
			if recurseErr != nil {
				return recurseErr
			}
		}
		if err := root.Remove(entryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
