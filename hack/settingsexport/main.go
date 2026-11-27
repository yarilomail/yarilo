// settingsexport prints every yarilo.yaml key as JSON: full path, section, Go
// type and the value config.Defaults() gives it. The docs repo checks its data against it.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

type setting struct {
	Key     string `json:"key"`
	Section string `json:"section"`
	Type    string `json:"type"`
	Kind    string `json:"kind"`
	Default any    `json:"default"`
}

func main() {
	cfg := config.Defaults()
	// Read from the machine at start; the export would otherwise carry the runner's name.
	cfg.Hostname = ""
	// The walk yarctl config uses too: shapes, so a map of sections reads as
	// one "*" entry and a list as one "[]".
	var out []setting
	for _, s := range config.Walk(cfg, true) {
		out = append(out, setting{Key: s.Key, Section: s.Section, Type: s.Type, Kind: s.Kind, Default: s.Value})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "settingsexport:", err)
		os.Exit(1)
	}
}
