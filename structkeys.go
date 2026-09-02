package viperconfig

import (
	"reflect"
	"strings"
)

// structKey is one leaf configuration key found by walking a config struct:
// the dotted viper path plus whether its destination field is a []string.
type structKey struct {
	path string
	// stringSlice marks a field a CSV string can legally be split into.
	stringSlice bool
}

// structKeys walks the config struct TYPE and returns every leaf key path.
//
// Paths are built exactly like validate() builds them — the mapstructure tag,
// or the lowercased field name when there is no tag — so the keys handed to
// viper.BindEnv are the same keys validate() later reports on.
//
// It walks types rather than values so a nil *struct field still contributes
// its keys: mapstructure allocates it during Unmarshal if data shows up.
func structKeys(cfg any) []structKey {
	var out []structKey
	collectStructKeys(reflect.TypeOf(cfg), "", map[reflect.Type]bool{}, &out)
	return out
}

func collectStructKeys(t reflect.Type, prefix string, onPath map[reflect.Type]bool, out *[]structKey) {
	t = deref(t)
	if t == nil || t.Kind() != reflect.Struct {
		return
	}
	// A struct that points back at itself (a linked config node) would recurse
	// forever; one visit per type per path is enough to enumerate its keys.
	if onPath[t] {
		return
	}
	onPath[t] = true
	defer delete(onPath, t)

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			// Unexported: Unmarshal cannot write it, so it has no key.
			continue
		}
		name, squash := parseMapstructureTag(field)
		if name == "-" {
			continue
		}

		ft := deref(field.Type)
		isStruct := ft != nil && ft.Kind() == reflect.Struct

		// `,squash` hoists an embedded struct's fields to the parent level, so
		// its children keep the parent's prefix instead of nesting under a key.
		if isStruct && squash {
			collectStructKeys(ft, prefix, onPath, out)
			continue
		}

		key := name
		if prefix != "" {
			key = prefix + "." + name
		}

		if isStruct && ft.NumField() > 0 && hasExportedField(ft) {
			collectStructKeys(ft, key, onPath, out)
			continue
		}

		*out = append(*out, structKey{
			path:        key,
			stringSlice: ft != nil && ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.String,
		})
	}
}

// parseMapstructureTag returns the key name for a field and whether the tag
// carries the ",squash" modifier. An absent tag falls back to the lowercased
// field name, matching validate().
func parseMapstructureTag(field reflect.StructField) (name string, squash bool) {
	tag := field.Tag.Get("mapstructure")
	name, opts, _ := strings.Cut(tag, ",")
	if name == "" && !strings.HasPrefix(tag, ",") {
		name = strings.ToLower(field.Name)
	}
	if name == "" {
		name = strings.ToLower(field.Name)
	}
	for _, opt := range strings.Split(opts, ",") {
		if opt == "squash" {
			squash = true
		}
	}
	return name, squash
}

func deref(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t
}

// hasExportedField keeps types like time.Time — a struct whose fields are all
// unexported — treated as leaves instead of being walked into.
func hasExportedField(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).PkgPath == "" {
			return true
		}
	}
	return false
}
