package faultfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

var ErrIdentityChanged = errors.New("faultfs: filesystem identity changed")

type FileIdentity struct {
	Device uint64
	Inode  uint64
}

type Publication struct {
	DestinationParent string
	DestinationPath   string
}

type AnchoredPublisher interface {
	PublishNoReplaceAnchored(Point, Point, string, string, string, string, FileIdentity) (Publication, error)
}

func PublishNoReplaceAnchored(fsys FS, renamePoint, syncPoint Point, oldParent, oldName, newParent, newName string, expected FileIdentity) (Publication, error) {
	publisher, ok := fsys.(AnchoredPublisher)
	if !ok {
		return Publication{}, ErrIdentityChanged
	}
	return publisher.PublishNoReplaceAnchored(renamePoint, syncPoint, oldParent, oldName, newParent, newName, expected)
}

type File interface {
	io.Reader
	io.Writer
	io.Seeker
	Stat() (fs.FileInfo, error)
	Sync() error
	Truncate(int64) error
	Close() error
	Fd() uintptr
}

type Flusher interface {
	Flush() error
}

type FS interface {
	OpenFile(Point, string, int, fs.FileMode) (File, error)
	Write(Point, io.Writer, []byte) (int, error)
	Flush(Point, Flusher) error
	Sync(Point, interface{ Sync() error }) error
	Rename(Point, string, string) error
	SyncDir(Point, string) error
	Truncate(Point, interface{ Truncate(int64) error }, int64) error
	Remove(Point, string) error
	Stat(Point, string) (fs.FileInfo, error)
	ReadDir(Point, string) ([]fs.DirEntry, error)
}

// Checkpoint exposes non-filesystem authority boundaries to the same
// deterministic hook used by filesystem operations. Implementations that do
// not provide checkpoints remain usable and simply do not inject at them.
func Checkpoint(fsys FS, point Point, phase Phase) error {
	checkpoint, ok := fsys.(interface {
		Checkpoint(Point, Phase) error
	})
	if !ok {
		return nil
	}
	return checkpoint.Checkpoint(point, phase)
}

type OS struct {
	hook Hook
}

func NewOS(hook Hook) *OS { return &OS{hook: hook} }

func (o *OS) Checkpoint(point Point, phase Phase) error { return o.visit(point, phase) }

func (o *OS) visit(point Point, phase Phase) error {
	if o == nil || o.hook == nil {
		return nil
	}
	event := Event{Point: point, Phase: phase}
	if err := o.hook.Visit(event); err != nil {
		return &HookError{Event: event, Err: err}
	}
	return nil
}

func (o *OS) OpenFile(point Point, name string, flag int, perm fs.FileMode) (File, error) {
	if err := o.visit(point, Before); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(name, flag, perm)
	if err != nil {
		return nil, err
	}
	if err := o.visit(point, After); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func (o *OS) Write(point Point, writer io.Writer, value []byte) (int, error) {
	if err := o.visit(point, Before); err != nil {
		return 0, err
	}
	written, err := writer.Write(value)
	if err != nil {
		return written, err
	}
	if err := o.visit(point, After); err != nil {
		return written, err
	}
	return written, nil
}

func (o *OS) Flush(point Point, flusher Flusher) error {
	if err := o.visit(point, Before); err != nil {
		return err
	}
	if err := flusher.Flush(); err != nil {
		return err
	}
	return o.visit(point, After)
}

func (o *OS) Sync(point Point, syncer interface{ Sync() error }) error {
	if err := o.visit(point, Before); err != nil {
		return err
	}
	if err := syncer.Sync(); err != nil {
		return err
	}
	return o.visit(point, After)
}

func (o *OS) Rename(point Point, oldPath, newPath string) error {
	if err := o.visit(point, Before); err != nil {
		return err
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return err
	}
	return o.visit(point, After)
}

// PublishNoReplaceAnchored pins both parents through rename, moved-inode
// verification, and fsync of the directories whose entries changed.
func (o *OS) PublishNoReplaceAnchored(renamePoint, syncPoint Point, oldParent, oldName, newParent, newName string, expected FileIdentity) (Publication, error) {
	oldFD, err := unix.Open(oldParent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return Publication{}, err
	}
	defer unix.Close(oldFD)
	newFD, err := unix.Open(newParent, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return Publication{}, err
	}
	defer unix.Close(newFD)
	var oldParentStat, newParentStat unix.Stat_t
	if err := unix.Fstat(oldFD, &oldParentStat); err != nil {
		return Publication{}, err
	}
	if err := unix.Fstat(newFD, &newParentStat); err != nil {
		return Publication{}, err
	}
	if err := o.visit(renamePoint, Before); err != nil {
		return Publication{}, err
	}
	if !pathHasIdentity(oldParent, uint64(oldParentStat.Dev), oldParentStat.Ino) || !pathHasIdentity(newParent, uint64(newParentStat.Dev), newParentStat.Ino) {
		return Publication{}, ErrIdentityChanged
	}
	var source unix.Stat_t
	if err := unix.Fstatat(oldFD, oldName, &source, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return Publication{}, err
	}
	if uint64(source.Dev) != expected.Device || source.Ino != expected.Inode {
		return Publication{}, ErrIdentityChanged
	}
	if err := unix.Renameat2(oldFD, oldName, newFD, newName, unix.RENAME_NOREPLACE); err != nil {
		return Publication{}, err
	}
	publication := publicationLocation(newFD, newName)
	var moved unix.Stat_t
	if err := unix.Fstatat(newFD, newName, &moved, unix.AT_SYMLINK_NOFOLLOW); err != nil || uint64(moved.Dev) != expected.Device || moved.Ino != expected.Inode {
		if err != nil {
			return publication, appliedPublicationError(renamePoint, err)
		}
		return publication, appliedPublicationError(renamePoint, ErrIdentityChanged)
	}
	if err := o.syncPublishedDirectory(renamePoint, syncPoint, oldFD); err != nil {
		return publicationLocation(newFD, newName), err
	}
	if oldParentStat.Dev != newParentStat.Dev || oldParentStat.Ino != newParentStat.Ino {
		if err := o.syncPublishedDirectory(renamePoint, syncPoint, newFD); err != nil {
			return publicationLocation(newFD, newName), err
		}
	}
	if err := o.visit(renamePoint, After); err != nil {
		return publicationLocation(newFD, newName), err
	}
	publication = publicationLocation(newFD, newName)
	if err := unix.Fstatat(newFD, newName, &moved, unix.AT_SYMLINK_NOFOLLOW); err != nil || uint64(moved.Dev) != expected.Device || moved.Ino != expected.Inode {
		if err != nil {
			return publication, appliedPublicationError(renamePoint, err)
		}
		return publication, appliedPublicationError(renamePoint, ErrIdentityChanged)
	}
	if !pathHasIdentity(oldParent, uint64(oldParentStat.Dev), oldParentStat.Ino) || !pathHasIdentity(newParent, uint64(newParentStat.Dev), newParentStat.Ino) {
		return publication, appliedPublicationError(renamePoint, ErrIdentityChanged)
	}
	return publication, nil
}

func (o *OS) syncPublishedDirectory(renamePoint, syncPoint Point, fd int) error {
	if err := o.visit(syncPoint, Before); err != nil {
		return appliedPublicationError(renamePoint, err)
	}
	if err := unix.Fsync(fd); err != nil {
		return appliedPublicationError(renamePoint, err)
	}
	if err := o.visit(syncPoint, After); err != nil {
		return appliedPublicationError(renamePoint, err)
	}
	return nil
}

func publicationLocation(parentFD int, name string) Publication {
	parent, err := os.Readlink("/proc/self/fd/" + fmt.Sprint(parentFD))
	if err != nil || !filepath.IsAbs(parent) {
		return Publication{}
	}
	parent = filepath.Clean(parent)
	return Publication{DestinationParent: parent, DestinationPath: filepath.Join(parent, name)}
}

func appliedPublicationError(point Point, err error) error {
	return &HookError{Event: Event{Point: point, Phase: After}, Err: err}
}

func pathHasIdentity(path string, device uint64, inode uint64) bool {
	info, err := os.Lstat(filepath.Clean(path))
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(stat.Dev) == device && stat.Ino == inode
}

func (o *OS) SyncDir(point Point, path string) error {
	if err := o.visit(point, Before); err != nil {
		return err
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return o.visit(point, After)
}

func (o *OS) Truncate(point Point, file interface{ Truncate(int64) error }, size int64) error {
	if err := o.visit(point, Before); err != nil {
		return err
	}
	if err := file.Truncate(size); err != nil {
		return err
	}
	return o.visit(point, After)
}

func (o *OS) Remove(point Point, path string) error {
	if err := o.visit(point, Before); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return o.visit(point, After)
}

func (o *OS) Stat(point Point, path string) (fs.FileInfo, error) {
	if err := o.visit(point, Before); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if err := o.visit(point, After); err != nil {
		return nil, err
	}
	return info, nil
}

func (o *OS) ReadDir(point Point, path string) ([]fs.DirEntry, error) {
	if err := o.visit(point, Before); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	if err := o.visit(point, After); err != nil {
		return nil, err
	}
	return entries, nil
}
