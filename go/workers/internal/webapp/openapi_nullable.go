package webapp

import (
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// markNullableObjects makes the spec say what encoding/json sends: a pointer
// to a struct without omitempty marshals as null when nil, but huma documents
// object refs as never null. Without this the generated client types promise
// an object the server can omit, and a client that trusts them crashes.
func markNullableObjects(spec *huma.OpenAPI) {
	registry := spec.Components.Schemas
	for name, schema := range registry.Map() {
		t := registry.TypeFromRef("#/components/schemas/" + name)
		if t == nil || schema.Properties == nil {
			continue
		}
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			continue
		}
		markNullableFields(t, schema)
	}
}

func markNullableFields(t reflect.Type, schema *huma.Schema) {
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous {
			ft := f.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				markNullableFields(ft, schema)
			}
			continue
		}
		tag := f.Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		if name == "-" || strings.Contains(opts, "omitempty") || f.Type.Kind() != reflect.Pointer {
			continue
		}
		if name == "" {
			name = f.Name
		}
		prop := schema.Properties[name]
		if prop == nil || prop.Ref == "" {
			continue
		}
		schema.Properties[name] = &huma.Schema{AnyOf: []*huma.Schema{{Ref: prop.Ref}, {Type: "null"}}}
	}
}
