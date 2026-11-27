package config

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// Setting is one key of the configuration as Walk sees it.
type Setting struct {
	// Key is the dotted path; a map entry's name or "*", a list entry "[i]"
	// or "[]".
	Key     string
	Section string
	Type    string
	Kind    string
	Value   any
	// Secret marks a value that is a credential: tag `secret:"true"`.
	Secret bool
	// Alias is a deprecated spelling the loader copies into its canonical
	// key; an operator's view leaves it out.
	Alias bool
	// path is Key in segments, for building the tree.
	path []segment
}

type segment struct {
	name  string
	index int // >= 0 for a list entry
}

// Walk lists every key of cfg by koanf tag. With shapes, a map or list of
// sections is one "*" or "[]" entry of zero values: every key there can be,
// which is what the settings export documents. Without, it is the entries
// cfg actually holds, which is what an operator is shown.
func Walk(cfg *Config, shapes bool) []Setting {
	var out []Setting
	walkStruct(nil, reflect.ValueOf(cfg).Elem(), false, shapes, &out)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func walkStruct(path []segment, v reflect.Value, secret, shapes bool, out *[]Setting) {
	walkStructAlias(path, v, secret, false, shapes, out)
}

func walkStructAlias(path []segment, v reflect.Value, secret, alias, shapes bool, out *[]Setting) {
	t := v.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("koanf"), ",")
		if !f.IsExported() || name == "" || name == "-" {
			continue
		}
		p := append(append([]segment(nil), path...), segment{name: name, index: -1})
		walkValue(p, v.Field(i), secret || f.Tag.Get("secret") == "true", alias || strings.HasSuffix(f.Name, "Alias"), shapes, out)
	}
}

func walkValue(path []segment, v reflect.Value, secret, alias, shapes bool, out *[]Setting) {
	t := v.Type()
	switch {
	case t.Kind() == reflect.Struct:
		walkStructAlias(path, v, secret, alias, shapes, out)
	case t.Kind() == reflect.Pointer && t.Elem().Kind() == reflect.Struct:
		if v.IsNil() {
			if !shapes {
				// Unset, which is not the same as set to zero values: shown
				// as null, which loads back unset.
				*out = append(*out, Setting{
					Key: keyOf(path), Section: sectionOf(path), Type: t.String(), Kind: "section",
					Value: nil, Secret: secret, Alias: alias, path: path,
				})
				return
			}
			v = reflect.New(t.Elem())
		}
		walkStructAlias(path, v.Elem(), secret, alias, shapes, out)
	case t.Kind() == reflect.Map && t.Elem().Kind() == reflect.Struct:
		if shapes {
			walkStructAlias(append(path, segment{name: "*", index: -1}), reflect.New(t.Elem()).Elem(), secret, alias, shapes, out)
			return
		}
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		for _, k := range keys {
			walkStructAlias(append(append([]segment(nil), path...), segment{name: k.String(), index: -1}), v.MapIndex(k), secret, alias, shapes, out)
		}
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Struct:
		if shapes {
			walkStructAlias(append(path, segment{name: "[]", index: -1}), reflect.New(t.Elem()).Elem(), secret, alias, shapes, out)
			return
		}
		for i := range v.Len() {
			walkStructAlias(append(append([]segment(nil), path...), segment{index: i}), v.Index(i), secret, alias, shapes, out)
		}
	default:
		*out = append(*out, Setting{
			Key: keyOf(path), Section: sectionOf(path), Type: t.String(), Kind: kindOf(t),
			Value: v.Interface(), Secret: secret, Alias: alias, path: path,
		})
	}
}

func keyOf(path []segment) string {
	var b strings.Builder
	for i, s := range path {
		if s.index >= 0 {
			b.WriteString("[" + strconv.Itoa(s.index) + "]")
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s.name)
	}
	return strings.ReplaceAll(b.String(), ".[]", "[]")
}

// sectionOf is the key without its last name: what holds it.
func sectionOf(path []segment) string {
	if len(path) == 0 {
		return ""
	}
	return keyOf(path[:len(path)-1])
}

// kindOf is the type's base shape, so a named type (OAuth2Mode) reads as string.
func kindOf(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Pointer:
		return kindOf(t.Elem())
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

// Hidden stands for a secret value an operator did not ask to see.
const Hidden = "<hidden>"

// Tree nests settings back into the shape of yarilo.yaml: a map per section,
// a list for list entries. Secrets are Hidden unless showSecrets.
func Tree(settings []Setting, showSecrets bool) map[string]any {
	root := map[string]any{}
	for _, s := range settings {
		val := s.Value
		if s.Secret && !showSecrets && !isZero(val) {
			val = Hidden
		}
		insert(root, s.path, val)
	}
	return root
}

func isZero(v any) bool { return v == nil || reflect.ValueOf(v).IsZero() }

// insert places val at path; a list entry is a map inside a []any.
func insert(node map[string]any, path []segment, val any) {
	head := path[0]
	if len(path) == 1 {
		node[head.name] = val
		return
	}
	next := path[1]
	if next.index >= 0 {
		list, _ := node[head.name].([]any)
		for len(list) <= next.index {
			list = append(list, map[string]any{})
		}
		node[head.name] = list
		entry, _ := list[next.index].(map[string]any)
		if len(path) == 2 {
			return
		}
		insert(entry, path[2:], val)
		return
	}
	child, ok := node[head.name].(map[string]any)
	if !ok {
		child = map[string]any{}
		node[head.name] = child
	}
	insert(child, path[1:], val)
}
