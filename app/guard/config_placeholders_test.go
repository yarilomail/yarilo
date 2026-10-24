package guard_test

import (
	"fmt"
	"reflect"
	"regexp"
)

var placeholder = regexp.MustCompile(`\$\{([A-Z0-9_]+)\}`)

// unexpanded lists every string under v that still holds "${", by path.
func unexpanded(v reflect.Value, path string, out *[]string) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			unexpanded(v.Elem(), path, out)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).IsExported() {
				unexpanded(v.Field(i), path+"."+v.Type().Field(i).Name, out)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			unexpanded(v.Index(i), fmt.Sprintf("%s[%d]", path, i), out)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			unexpanded(v.MapIndex(k), fmt.Sprintf("%s[%v]", path, k), out)
		}
	case reflect.String:
		if placeholder.MatchString(v.String()) {
			*out = append(*out, path)
		}
	}
}
