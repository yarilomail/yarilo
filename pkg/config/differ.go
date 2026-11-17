package config

import "reflect"

// Differing is what the file changes against the defaults. A key the defaults
// lack (a field of a list or map entry they do not have) is compared with its
// zero value: an entry shows only the fields it sets, and null, "not set",
// is not printed.
func Differing(in, defaults []Setting) []Setting {
	def := make(map[string]any, len(defaults))
	for _, s := range defaults {
		def[s.Key] = s.Value
	}
	var out []Setting
	for _, s := range in {
		d, ok := def[s.Key]
		if ok && !SameValue(d, s.Value) || !ok && !emptyValue(s.Value) {
			out = append(out, s)
		}
	}
	return out
}

// SameValue is DeepEqual with an empty list or map equal to a nil one: both
// load from the same file, and YAML writes both as [] or {}.
func SameValue(a, b any) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	if va.IsValid() && vb.IsValid() && va.Type() == vb.Type() &&
		(va.Kind() == reflect.Slice || va.Kind() == reflect.Map) {
		return va.Len() == 0 && vb.Len() == 0
	}
	return false
}

func emptyValue(v any) bool {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() || rv.IsZero() {
		return true
	}
	return (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Map) && rv.Len() == 0
}
