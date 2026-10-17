package greenfield

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"censi/harness/internal/tools"
)

var laravelVersion = regexp.MustCompile(`^\d+(?:\.\d+)?\.\*$`)

func (r *Runtime) bootstrap(ctx context.Context, version string) tools.Result {
	if !laravelVersion.MatchString(version) {
		return tools.Error("bootstrap requires a confirmed Laravel version, e.g. 13.*")
	}
	marker, e := r.Store.Path("bootstrap.json")
	if e != nil {
		return tools.Error(e.Error())
	}

	raw, e := os.ReadFile(marker)
	owned := false
	if e == nil {
		var saved map[string]string
		if json.Unmarshal(raw, &saved) != nil {
			return tools.Error("invalid bootstrap marker")
		}
		if saved["version"] != version {
			return tools.Error("bootstrap version differs from the existing workflow skeleton; revise the plan rather than replacing it")
		}
		owned = true
		if saved["status"] != "copying" && skeletonReady(r.Store.Root) {
			return tools.Text("Laravel skeleton already created and verified by this workflow")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return tools.Error(e.Error())
	}
	if !owned {
		if _, err := os.Lstat(filepath.Join(r.Store.Root, "composer.json")); !errors.Is(err, os.ErrNotExist) {
			return tools.Error("bootstrap will not replace an existing application with composer.json")
		}
	}
	staging := fmt.Sprintf(".maxlabs/team/scaffold-%d", time.Now().UnixNano())
	command := fmt.Sprintf("composer create-project laravel/laravel \"%s\" \"%s\" --no-install --no-interaction --no-scripts", staging, version)
	result := r.executeCommand(ctx, command, false)
	if result.IsError {
		return result
	}
	source, e := SafePath(r.Store.Root, staging)
	if e != nil {
		return tools.Error(e.Error())
	}
	defer os.RemoveAll(source)
	if !skeletonReady(source) {
		return tools.Error("Composer did not produce a complete Laravel skeleton (composer.json, artisan, bootstrap/app.php)")
	}
	raw, _ = json.Marshal(map[string]string{"version": version, "status": "copying"})
	if e = os.WriteFile(marker, raw, 0600); e != nil {
		return tools.Error(e.Error())
	}
	e = filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == source {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.Name() == ".git" || entry.Name() == ".maxlabs" || (entry.Name() != ".env.example" && (entry.Name() == ".env" || strings.HasPrefix(entry.Name(), ".env."))) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination, err := SafePath(r.Store.Root, rel)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(destination, 0755)
		}
		if info, err := os.Lstat(destination); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("bootstrap destination %s is not a regular file", rel)
			}
			return nil // a retry fills missing framework files; it never overwrites worker changes
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		input, err := os.Open(path)
		if err != nil {
			return err
		}
		defer input.Close()
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		_, err = io.Copy(output, input)
		closed := output.Close()
		if err != nil {
			return err
		}
		return closed
	})
	if e != nil {
		return tools.Error(e.Error())
	}

	if raw, e := os.ReadFile(filepath.Join(r.Store.Root, ".env.example")); e == nil {
		key := make([]byte, 32)
		if _, e = rand.Read(key); e != nil {
			return tools.Error(e.Error())
		}
		environment := regexp.MustCompile(`(?m)^APP_KEY=.*$`).ReplaceAllString(string(raw), "APP_KEY=base64:"+base64.StdEncoding.EncodeToString(key))
		envFile, e := os.OpenFile(filepath.Join(r.Store.Root, ".env"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e == nil {
			_, e = envFile.WriteString(environment)
			closed := envFile.Close()
			if e != nil {
				return tools.Error(e.Error())
			}
			if closed != nil {
				return tools.Error(closed.Error())
			}
		} else if !errors.Is(e, os.ErrExist) {
			return tools.Error(e.Error())
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return tools.Error(e.Error())
	}
	database, e := os.OpenFile(filepath.Join(r.Store.Root, "database", "database.sqlite"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e == nil {
		_ = database.Close()
	} else if !errors.Is(e, os.ErrExist) && !errors.Is(e, os.ErrNotExist) {
		return tools.Error(e.Error())
	}
	raw, _ = json.Marshal(map[string]string{"version": version, "status": "complete"})
	if e = os.WriteFile(marker, raw, 0600); e != nil {
		return tools.Error(e.Error())
	}
	return tools.Text("Created official Laravel " + version + " skeleton, preserving existing files. Dependencies are installed by approved task checks.")
}

// A marker alone is not evidence that a skeleton exists.
func skeletonReady(root string) bool {
	for _, name := range []string{"composer.json", "artisan", "bootstrap/app.php"} {
		path, err := SafePath(root, name)
		if err != nil {
			return false
		}
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}
