package config

import (
	"fmt"
	"log/slog"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// unknownKeys are the keys of the file no field of Config is tagged with: a
// typo, or a key from another version. The free-form maps (dict settings,
// passdb fields, Sieve environments) take any key; a retired key is known,
// and warned about elsewhere.
func unknownKeys(k *koanf.Koanf) []string {
	retired := map[string]bool{}
	for _, r := range retiredKeys() {
		retired[r.key] = true
	}
	var out []string
	walkUnknown(k.Raw(), reflect.TypeOf(Config{}), "", retired, &out)
	sort.Strings(out)
	return out
}

func walkUnknown(raw map[string]any, t reflect.Type, prefix string, retired map[string]bool, out *[]string) {
	fields := map[string]reflect.Type{}
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("koanf"), ",")
		if f.IsExported() && name != "" && name != "-" {
			fields[name] = f.Type
		}
	}
	for key, val := range raw {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		ft, ok := fields[key]
		if !ok {
			if !retired[path] {
				*out = append(*out, path)
			}
			continue
		}
		walkUnknownValue(val, ft, path, retired, out)
	}
}

func walkUnknownValue(val any, t reflect.Type, path string, retired map[string]bool, out *[]string) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t.Kind() == reflect.Struct:
		if m, ok := val.(map[string]any); ok {
			walkUnknown(m, t, path, retired, out)
		}
	case t.Kind() == reflect.Map && structOf(t.Elem()) != nil:
		if m, ok := val.(map[string]any); ok {
			for name, entry := range m {
				walkUnknownValue(entry, t.Elem(), path+"."+name, retired, out)
			}
		}
	case t.Kind() == reflect.Slice && structOf(t.Elem()) != nil:
		if list, ok := val.([]any); ok {
			for i, entry := range list {
				walkUnknownValue(entry, t.Elem(), path+"["+strconv.Itoa(i)+"]", retired, out)
			}
		}
	}
}

func structOf(t reflect.Type) reflect.Type {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Struct {
		return t
	}
	return nil
}

// refuseUnknownKeys stops the load on a key nothing reads: a typo, whose
// setting would otherwise run at its default (#2567). From a file a newer
// chart wrote (a schema above this binary's) the keys are that version's: a
// pod still on the older image during a rollout, or one pinned below the
// chart, keeps starting and names them.
func refuseUnknownKeys(unknown []string, fileSchema int) error {
	if len(unknown) == 0 {
		return nil
	}
	if fileSchema > minConfigSchema {
		slog.Warn("config: keys this binary does not read, from a newer chart; ignored",
			"file_schema", fileSchema, "binary_schema", minConfigSchema, "keys", strings.Join(unknown, ","))
		return nil
	}
	return fmt.Errorf("config: unknown key(s), read by nothing: %s", strings.Join(unknown, ", "))
}

// UnknownKeys reads the file at path and returns its unknown keys.
func UnknownKeys(path string) ([]string, error) {
	k := koanf.New(".")
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return nil, err
	}
	return unknownKeys(k), nil
}
