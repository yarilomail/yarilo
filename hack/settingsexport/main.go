// settingsexport prints every yarilo.yaml key as JSON: full path, section, Go
// type and the value config.Defaults() gives it. The docs repo checks its data against it.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/yarilomail/yarilo/v2/pkg/config"
)

type setting struct {
	Key     string `json:"key"`
	Section string `json:"section"`
	Type    string `json:"type"`
	Kind    string `json:"kind"`
	Default any    `json:"default"`
}

// collect walks v by koanf tag. A map entry's name is "*", a list entry's "[]".
func collect(prefix string, v reflect.Value, out *[]setting) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("koanf"), ",")
		if !f.IsExported() || name == "" || name == "-" {
			continue
		}
		key := name
		if prefix != "" {
			key = prefix + "." + name
		}
		add(prefix, key, v.Field(i), out)
	}
}

func add(section, key string, fv reflect.Value, out *[]setting) {
	ft := fv.Type()
	switch {
	case ft.Kind() == reflect.Struct:
		collect(key, fv, out)
	case ft.Kind() == reflect.Pointer && ft.Elem().Kind() == reflect.Struct:
		if fv.IsNil() {
			fv = reflect.New(ft.Elem())
		}
		collect(key, fv.Elem(), out)
	case ft.Kind() == reflect.Map && ft.Elem().Kind() == reflect.Struct:
		collect(key+".*", reflect.New(ft.Elem()).Elem(), out)
	case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
		collect(key+"[]", reflect.New(ft.Elem()).Elem(), out)
	default:
		*out = append(*out, setting{Key: key, Section: section, Type: ft.String(), Kind: kind(ft), Default: fv.Interface()})
	}
}

// kind is the type's base shape, so a named type (config.OAuth2Mode) still reads as string.
func kind(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Pointer:
		return kind(t.Elem())
	case reflect.Slice:
		return "list"
	case reflect.Map:
		return "map"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Bool:
		return "boolean"
	}
	return t.Kind().String()
}

func main() {
	cfg := config.Defaults()
	// Read from the machine at start; the export would otherwise carry the runner's name.
	cfg.Hostname = ""
	var out []setting
	collect("", reflect.ValueOf(cfg).Elem(), &out)
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, "settingsexport:", err)
		os.Exit(1)
	}
}
