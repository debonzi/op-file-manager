// Package localfs confines browsing and transfers to a selected local root.
package localfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Root struct {
	path string
}

type Entry struct {
	Name      string
	IsDir     bool
	IsSymlink bool
	Path      string
	Size      int64
	Modified  time.Time
	Mode      fs.FileMode
}

func NewRoot(directory string) (Root, error) {
	abs, err := filepath.Abs(directory)
	if err != nil {
		return Root{}, fmt.Errorf("resolve root: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Root{}, fmt.Errorf("resolve root symlinks: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return Root{}, fmt.Errorf("inspect root: %w", err)
	}
	if !info.IsDir() {
		return Root{}, errors.New("root must be a directory")
	}
	return Root{path: resolved}, nil
}

func (r Root) Path() string { return r.path }

func (r Root) Resolve(relative string) (string, error) {
	if filepath.IsAbs(relative) {
		return "", errors.New("local path must be relative to the selected root")
	}
	candidate := filepath.Clean(filepath.Join(r.path, relative))
	rel, err := filepath.Rel(r.path, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("local path escapes the selected root")
	}
	return candidate, nil
}

func (r Root) List(relative string) ([]Entry, error) {
	directory, err := r.Resolve(relative)
	if err != nil {
		return nil, err
	}
	items, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read local directory: %w", err)
	}
	entries := make([]Entry, 0, len(items))
	for _, item := range items {
		itemPath := filepath.Join(directory, item.Name())
		info, err := item.Info()
		if err != nil {
			return nil, fmt.Errorf("inspect local entry %q: %w", item.Name(), err)
		}
		entries = append(entries, Entry{
			Name:      item.Name(),
			IsDir:     info.IsDir(),
			IsSymlink: item.Type()&fs.ModeSymlink != 0,
			Path:      itemPath,
			Size:      info.Size(),
			Modified:  info.ModTime(),
			Mode:      info.Mode(),
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, nil
}

// CheckSourceFile accepts regular files only and rejects symlinks even when
// their targets are inside the root.
func (r Root) CheckSourceFile(absolute string) error {
	rel, err := filepath.Rel(r.path, absolute)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("source escapes the selected root")
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return fmt.Errorf("inspect source: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return errors.New("symbolic links cannot be copied")
	}
	if !info.Mode().IsRegular() {
		return errors.New("only regular files can be copied")
	}
	return nil
}

// CheckDestination prevents a remote download from traversing an existing
// symlink. The caller decides whether replacing a regular file is confirmed.
func (r Root) CheckDestination(absolute string) (exists bool, err error) {
	rel, relErr := filepath.Rel(r.path, absolute)
	if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, errors.New("destination escapes the selected root")
	}
	info, err := os.Lstat(absolute)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect destination: %w", err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return true, errors.New("refusing to overwrite a symbolic link")
	}
	if info.IsDir() || !info.Mode().IsRegular() {
		return true, errors.New("destination is not a regular file")
	}
	return true, nil
}
