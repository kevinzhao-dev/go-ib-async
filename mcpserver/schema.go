package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// Tool argument structs describe their JSON schema with struct tags:
//
//	json:"name,omitempty"  property name (required for every field)
//	desc:"..."             description shown to the model
//	enum:"A,B,C"           allowed values (strings and []string items);
//	                       matched case-insensitively and canonicalised
//	required:"true"        must be present and non-zero
//
// The same tags drive validation, so the advertised schema and the checks
// cannot drift apart.

// schemaFor returns the JSON schema of a tool argument struct type.
func schemaFor(t reflect.Type) map[string]any {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		props := map[string]any{}
		var required []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := jsonName(f)
			if name == "" {
				continue
			}
			p := schemaFor(f.Type)
			if d := f.Tag.Get("desc"); d != "" {
				p["description"] = d
			}
			if e := enumOf(f); e != nil {
				if f.Type.Kind() == reflect.Slice {
					p["items"].(map[string]any)["enum"] = e
				} else {
					p["enum"] = e
				}
			}
			props[name] = p
			if f.Tag.Get("required") == "true" {
				required = append(required, name)
			}
		}
		s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	case reflect.Slice:
		return map[string]any{"type": "array", "items": schemaFor(t.Elem())}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer"}
	case reflect.Float64:
		return map[string]any{"type": "number"}
	}
	panic(fmt.Sprintf("mcpserver: unsupported argument type %s", t))
}

func jsonName(f reflect.StructField) string {
	if !f.IsExported() {
		return ""
	}
	name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if name == "-" {
		return ""
	}
	if name == "" {
		panic(fmt.Sprintf("mcpserver: argument field %s needs a json tag", f.Name))
	}
	return name
}

func enumOf(f reflect.StructField) []string {
	e := f.Tag.Get("enum")
	if e == "" {
		return nil
	}
	return strings.Split(e, ",")
}

// decodeArgs strictly decodes tool arguments into dst (a pointer to an
// argument struct) and validates them against the struct tags.
func decodeArgs(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(dst); err != nil {
			return fmt.Errorf("invalid arguments: %w", err)
		}
	}
	return validate(reflect.ValueOf(dst).Elem(), "")
}

func validate(v reflect.Value, prefix string) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name := jsonName(f)
		if name == "" {
			continue
		}
		path := prefix + name
		fv := v.Field(i)
		if f.Tag.Get("required") == "true" && fv.IsZero() {
			return fmt.Errorf("missing required argument %q", path)
		}
		if enum := enumOf(f); enum != nil {
			switch fv.Kind() {
			case reflect.String:
				if err := canonicalEnum(fv, enum, path); err != nil {
					return err
				}
			case reflect.Slice:
				for j := 0; j < fv.Len(); j++ {
					if err := canonicalEnum(fv.Index(j), enum, fmt.Sprintf("%s[%d]", path, j)); err != nil {
						return err
					}
				}
			}
		}
		switch {
		case fv.Kind() == reflect.Struct:
			if err := validate(fv, path+"."); err != nil {
				return err
			}
		case fv.Kind() == reflect.Pointer && fv.Type().Elem().Kind() == reflect.Struct && !fv.IsNil():
			if err := validate(fv.Elem(), path+"."); err != nil {
				return err
			}
		}
	}
	return nil
}

func canonicalEnum(v reflect.Value, enum []string, path string) error {
	s := v.String()
	if s == "" {
		return nil
	}
	for _, e := range enum {
		if strings.EqualFold(e, s) {
			v.SetString(e)
			return nil
		}
	}
	return fmt.Errorf("argument %q must be one of %s, got %q", path, strings.Join(enum, ", "), s)
}
