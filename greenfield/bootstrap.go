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
	if e == nil {
		var saved map[string]string
		if json.Unmarshal(raw, &saved) != nil {
			return tools.Error("invalid bootstrap marker")
		}
		if saved["version"] == version {
			return tools.Text("Laravel skeleton already created by this workflow")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return tools.Error(e.Error())
	}
	entries, e := os.ReadDir(r.Store.Root)
	if e != nil {
		return tools.Error(e.Error())
	}
	for _, entry := range entries {
		if entry.Name() != ".maxlabs" && entry.Name() != ".git" && entry.Name() != ".DS_Store" {
			return tools.Error("bootstrap requires an empty workspace; existing files are preserved")
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
		if entry.Name() == ".git" || entry.Name() == ".maxlabs" {
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
	raw, _ = json.Marshal(map[string]string{"version": version})
	if e = os.WriteFile(marker, raw, 0600); e != nil {
		return tools.Error(e.Error())
	}
	return tools.Text("Created official Laravel " + version + " skeleton. Dependencies are installed by approved task checks.")
}
