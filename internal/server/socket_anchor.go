package server

import (
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// directoryAnchor keeps socket publication and cleanup relative to one opened
// directory. Gapdb targets Unix/Linux; /proc/self/fd supplies an anchored bind
// pathname without changing the process working directory.
type directoryAnchor struct {
	directoryFD int
	parentFD    int
	name        string
}

type socketIdentity struct {
	device uint64
	inode  uint64
}

func openDirectoryAnchor(directory string) (*directoryAnchor, error) {
	clean := filepath.Clean(directory)
	directoryFD, err := unix.Open(clean, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	parentFD, err := unix.Open(filepath.Dir(clean), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		_ = unix.Close(directoryFD)
		return nil, err
	}
	anchor := &directoryAnchor{directoryFD: directoryFD, parentFD: parentFD, name: filepath.Base(clean)}
	if err := anchor.validatePath(); err != nil {
		_ = anchor.close()
		return nil, err
	}
	return anchor, nil
}

func (anchor *directoryAnchor) validatePath() error {
	if anchor == nil || anchor.directoryFD < 0 || anchor.parentFD < 0 || anchor.name == "" {
		return errors.New("socket directory anchor is closed")
	}
	var held, named unix.Stat_t
	if err := unix.Fstat(anchor.directoryFD, &held); err != nil {
		return err
	}
	if err := unix.Fstatat(anchor.parentFD, anchor.name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if held.Dev != named.Dev || held.Ino != named.Ino || held.Mode&unix.S_IFMT != unix.S_IFDIR || named.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("socket directory pathname no longer names its anchored inode")
	}
	return nil
}

func (anchor *directoryAnchor) bindPath(name string) (string, error) {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return "", errors.New("socket filename is invalid")
	}
	return fmt.Sprintf("/proc/self/fd/%d/%s", anchor.directoryFD, name), nil
}

func (anchor *directoryAnchor) removeSocket(name string) error {
	var stat unix.Stat_t
	err := unix.Fstatat(anchor.directoryFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return errors.New("existing anchored socket path is not a socket")
	}
	return unix.Unlinkat(anchor.directoryFD, name, 0)
}

func (anchor *directoryAnchor) secureSocket(name string) (socketIdentity, error) {
	if err := anchor.validatePath(); err != nil {
		return socketIdentity{}, err
	}
	var before, after unix.Stat_t
	if err := unix.Fstatat(anchor.directoryFD, name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return socketIdentity{}, err
	}
	if before.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return socketIdentity{}, errors.New("bound socket path is not a socket")
	}
	if err := unix.Fchmodat(anchor.directoryFD, name, 0o600, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return socketIdentity{}, err
	}
	if err := unix.Fstatat(anchor.directoryFD, name, &after, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return socketIdentity{}, err
	}
	if before.Dev != after.Dev || before.Ino != after.Ino || after.Mode&unix.S_IFMT != unix.S_IFSOCK || after.Mode&0o777 != 0o600 {
		return socketIdentity{}, errors.New("bound socket identity or mode changed")
	}
	return socketIdentity{device: uint64(after.Dev), inode: after.Ino}, nil
}

func (anchor *directoryAnchor) verifySocket(name string, identity socketIdentity) error {
	if err := anchor.validatePath(); err != nil {
		return err
	}
	var named unix.Stat_t
	if err := unix.Fstatat(anchor.directoryFD, name, &named, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if uint64(named.Dev) != identity.device || named.Ino != identity.inode || named.Mode&unix.S_IFMT != unix.S_IFSOCK || named.Mode&0o777 != 0o600 {
		return errors.New("published socket identity or mode changed")
	}
	return nil
}

func (anchor *directoryAnchor) close() error {
	if anchor == nil {
		return nil
	}
	directoryErr := unix.Close(anchor.directoryFD)
	parentErr := unix.Close(anchor.parentFD)
	anchor.directoryFD = -1
	anchor.parentFD = -1
	return errors.Join(directoryErr, parentErr)
}
