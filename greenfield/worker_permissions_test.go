package greenfield

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerAutoOutputCommands(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	_ = os.WriteFile(filepath.Join(root, ".env.example"), []byte("APP_NAME=Test"), 0600)
	for _, name := range []string{"escape", "quoted-escape"} {
		if e := os.Symlink(outside, filepath.Join(root, name)); e != nil {
			t.Fatal(e)
		}
	}
	_ = os.WriteFile(filepath.Join(outside, "secret"), []byte("private"), 0600)
	_ = os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link-to-secret.log"))
	raw, e := os.ReadFile("worker-command-cases.json")
	if e != nil {
		t.Fatal(e)
	}
	var cases []struct {
		Command string
		Auto    bool
	}
	if e = json.Unmarshal(raw, &cases); e != nil {
		t.Fatal(e)
	}
	for _, entry := range cases {
		command := strings.NewReplacer("{root}", root, "{tmp}", os.TempDir()).Replace(entry.Command)
		t.Run(command, func(t *testing.T) {
			g := &flexibleGuard{root: root}
			args, _ := json.Marshal(map[string]string{"command": command})
			reason, e := g.Permit(context.Background(), "verify", args, false, nil)
			if e != nil || (reason == "") != entry.Auto {
				assessed, _ := workerAutoCommand(root, command)
				t.Fatalf("auto %v expected %v, reason %s, assessed %s, error %v", reason == "", entry.Auto, reason, assessed, e)
			}
		})
	}
}
func TestWorkerTemplateCopyDoesNotOverwriteExistingEnvironment(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, ".env.example"), []byte("APP_NAME=Test"), 0600)
	_ = os.WriteFile(filepath.Join(root, ".env"), []byte("APP_KEY=existing"), 0600)
	command, _ := workerAutoCommand(root, "cp .env.example .env")
	if !strings.Contains(command, ".env") {
		t.Fatal("existing environment overwrite bypassed review", command)
	}
	command, ok := workerAutoCommand(root, "cp -n .env.example .env")
	if !ok || command != "true" {
		t.Fatal(command, ok)
	}
}
