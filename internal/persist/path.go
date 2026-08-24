package persist

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrUnsafeDestination = errors.New("destination path is unsafe")
	ErrDestinationExists = errors.New("destination entry already exists")
)

// ValidateNewDestination rejects aliases and requires an absent final directory
// entry. Symlink components are deliberately unsupported for administrative
// destinations so a later path walk cannot escape the validated ancestry.
func ValidateNewDestination(sourceRoot, destination string) error {
	if destination == "" || !filepath.IsAbs(destination) || filepath.Clean(destination) != destination {
		return fmt.Errorf("%w: destination must be absolute and clean", ErrUnsafeDestination)
	}
	if _, err := os.Lstat(destination); err == nil {
		return ErrDestinationExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return validateExternalParent(sourceRoot, filepath.Dir(destination))
}

// ValidateExternalDirectory accepts an existing real directory or atomically
// creates one after validating its real parent. It never accepts symlink aliases.
func ValidateExternalDirectory(sourceRoot, directory string) error {
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return fmt.Errorf("%w: directory must be absolute and clean", ErrUnsafeDestination)
	}
	info, err := os.Lstat(directory)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%w: destination directory is not a real directory", ErrUnsafeDestination)
		}
		return validateExternalParent(sourceRoot, directory)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ValidateNewDestination(sourceRoot, directory); err != nil {
		return err
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return err
	}
	return nil
}

func validateExternalParent(sourceRoot, parent string) error {
	resolvedSource, err := filepath.EvalSymlinks(sourceRoot)
	if err != nil {
		return err
	}
	resolvedSource, err = filepath.Abs(resolvedSource)
	if err != nil {
		return err
	}
	if err := rejectSymlinkComponents(parent); err != nil {
		return err
	}
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("%w: destination parent is missing or dangling", ErrUnsafeDestination)
	}
	resolvedParent, err = filepath.Abs(resolvedParent)
	if err != nil {
		return err
	}
	if withinPath(resolvedSource, resolvedParent) {
		return fmt.Errorf("%w: destination resolves inside the source", ErrUnsafeDestination)
	}
	return nil
}

func rejectSymlinkComponents(path string) error {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	current := volume + string(filepath.Separator)
	relative := strings.TrimPrefix(strings.TrimPrefix(clean, volume), string(filepath.Separator))
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("%w: destination ancestor is missing", ErrUnsafeDestination)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: destination contains a symlink component", ErrUnsafeDestination)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: destination ancestor is not a directory", ErrUnsafeDestination)
		}
	}
	return nil
}

func withinPath(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	return err == nil && (relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))))
}
