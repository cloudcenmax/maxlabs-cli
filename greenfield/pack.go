// Package greenfield contains the versioned contract shared by the npm CLI and
// desktop harness. JSON artifacts are deliberately YAML 1.2 compatible.
package greenfield

import (
	"embed"
	"encoding/json"
	"strings"
)

//go:embed pack.json
var files embed.FS

type Pack struct {
	SchemaVersion int                 `json:"schema_version"`
	Name          string              `json:"name"`
	Label         string              `json:"label"`
	Framework     string              `json:"framework"`
	Kinds         map[string][]string `json:"kinds"`
	FlexibleKinds map[string][]string `json:"flexible_kinds"`
	CheckPrefixes []string            `json:"check_prefixes"`
	Instructions  []string            `json:"instructions"`
}

var Contract = loadPack()

func loadPack() Pack {
	raw, _ := files.ReadFile("pack.json")
	var p Pack
	if err := json.Unmarshal(raw, &p); err != nil {
		panic(err)
	}
	return p
}
func Instructions() string { return strings.Join(Contract.Instructions, "\n\n") }
