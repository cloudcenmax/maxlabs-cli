package greenfield

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileHash fingerprints files or a bounded source directory. Directory records
// use the same UTF-8, NUL-delimited format as the CLI so approvals are portable.
func (s *Store) FileHash(path string) (string, error) {
	root, err := SafeWorkerPath(s.Root, path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if errors.Is(err, os.ErrNotExist) {
		return "missing", nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return "", fmt.Errorf("input %s is not a regular file", path)
		}
		raw, e := os.ReadFile(root)
		return hash(raw), e
	}
	records := map[string]string{}
	var bytes int64
	err = filepath.WalkDir(root, func(target string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if target == root {
			return nil
		}
		rel, e := filepath.Rel(root, target)
		if e != nil {
			return e
		}
		rel = filepath.ToSlash(rel)
		if len(records) >= 2000 {
			return fmt.Errorf("directory read %s exceeds 2000 entries; declare narrower read paths", path)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("directory read %s contains symlink %s; declare its safe target explicitly", path, rel)
		}
		workspaceRel, e := filepath.Rel(s.Root, target)
		if e != nil {
			return e
		}
		if _, e = SafeWorkerPath(s.Root, filepath.ToSlash(workspaceRel)); e != nil {
			return fmt.Errorf("directory read %s contains protected path %s", path, rel)
		}
		if entry.IsDir() {
			records[rel+"/"] = "directory"
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("directory read %s contains a non-regular file", path)
		}
		stat, e := entry.Info()
		if e != nil {
			return e
		}
		bytes += stat.Size()
		if bytes > 32*1024*1024 {
			return fmt.Errorf("directory read %s exceeds 32 MiB; declare narrower read paths", path)
		}
		raw, e := os.ReadFile(target)
		if e != nil {
			return e
		}
		records[rel] = hash(raw)
		return nil
	})
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(records))
	for name := range records {
		names = append(names, name)
	}
	sort.Strings(names)
	var payload strings.Builder
	payload.WriteString("directory-v1\x00")
	for _, name := range names {
		payload.WriteString(name)
		payload.WriteByte(0)
		payload.WriteString(records[name])
		payload.WriteByte(0)
	}
	return hash([]byte(payload.String())), nil
}
