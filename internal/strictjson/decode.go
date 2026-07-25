// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package strictjson decodes trusted evidence without JSON's duplicate-key
// last-wins ambiguity.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
)

// Decode rejects duplicate object keys, unknown fields, and trailing values.
func Decode(raw []byte, target any) error {
	if err := RejectDuplicateKeys(raw); err != nil {
		return err
	}
	if err := rejectNoncanonicalFields(raw, reflect.TypeOf(target)); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

// RejectDuplicateKeys recursively rejects repeated names in every JSON object.
func RejectDuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var consumeValue func() error
	consumeValue = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]bool)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("JSON object key is not a string")
				}
				if seen[key] {
					return fmt.Errorf("duplicate JSON object key %q", key)
				}
				for prior := range seen {
					if strings.EqualFold(prior, key) {
						return fmt.Errorf(
							"case-fold-colliding JSON object keys %q and %q", prior, key,
						)
					}
				}
				seen[key] = true
				if err := consumeValue(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil {
				return err
			}
			if end != json.Delim('}') {
				return errors.New("invalid JSON object")
			}
		case '[':
			for decoder.More() {
				if err := consumeValue(); err != nil {
					return err
				}
			}
			end, err := decoder.Token()
			if err != nil {
				return err
			}
			if end != json.Delim(']') {
				return errors.New("invalid JSON array")
			}
		default:
			return errors.New("unexpected closing JSON delimiter")
		}
		return nil
	}
	return consumeValue()
}

func rejectNoncanonicalFields(raw []byte, targetType reflect.Type) error {
	if targetType == nil {
		return errors.New("strict JSON target type is nil")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	return validateCanonicalValue(value, targetType)
}

func validateCanonicalValue(value any, targetType reflect.Type) error {
	for targetType.Kind() == reflect.Pointer {
		targetType = targetType.Elem()
	}
	if value == nil {
		return nil
	}
	switch typed := value.(type) {
	case map[string]any:
		switch targetType.Kind() {
		case reflect.Struct:
			fields := canonicalStructFields(targetType)
			for key, child := range typed {
				fieldType, ok := fields[key]
				if !ok {
					for canonical := range fields {
						if strings.EqualFold(canonical, key) {
							return fmt.Errorf(
								"noncanonical JSON object key %q (want %q)", key, canonical,
							)
						}
					}
					return fmt.Errorf("unknown field %q", key)
				}
				if err := validateCanonicalValue(child, fieldType); err != nil {
					return fmt.Errorf("%s: %w", key, err)
				}
			}
		case reflect.Map:
			for key, child := range typed {
				if err := validateCanonicalValue(child, targetType.Elem()); err != nil {
					return fmt.Errorf("%s: %w", key, err)
				}
			}
		case reflect.Interface:
			return nil
		}
	case []any:
		switch targetType.Kind() {
		case reflect.Slice, reflect.Array:
			for index, child := range typed {
				if err := validateCanonicalValue(child, targetType.Elem()); err != nil {
					return fmt.Errorf("[%d]: %w", index, err)
				}
			}
		}
	}
	return nil
}

func canonicalStructFields(targetType reflect.Type) map[string]reflect.Type {
	fields := make(map[string]reflect.Type)
	for targetType.Kind() == reflect.Pointer {
		targetType = targetType.Elem()
	}
	if targetType.Kind() != reflect.Struct {
		return fields
	}
	for index := 0; index < targetType.NumField(); index++ {
		field := targetType.Field(index)
		if !field.IsExported() {
			continue
		}
		tag := field.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		if field.Anonymous && name == "" {
			for embedded, embeddedType := range canonicalStructFields(field.Type) {
				fields[embedded] = embeddedType
			}
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field.Type
	}
	return fields
}
