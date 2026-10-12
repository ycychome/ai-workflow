package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

// validateSchemaDefinition accepts a deliberately small, predictable JSON
// Schema subset. It covers the portions needed for structured reports and
// function arguments: types, object properties/required/additionalProperties,
// and array items.
func validateSchemaDefinition(raw json.RawMessage) error {
	if !json.Valid(raw) {
		return errors.New("schema is not valid JSON")
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(raw, &schema); err != nil || schema == nil {
		return errors.New("schema must be an object")
	}
	types, err := schemaTypes(schema)
	if err != nil {
		return err
	}
	hasObject, hasArray := false, false
	for _, typ := range types {
		switch typ {
		case "object":
			hasObject = true
		case "array":
			hasArray = true
		}
	}
	if rawProperties, ok := schema["properties"]; ok {
		if !hasObject {
			return errors.New("properties requires type object")
		}
		var properties map[string]json.RawMessage
		if err := json.Unmarshal(rawProperties, &properties); err != nil || properties == nil {
			return errors.New("properties must be an object")
		}
		for name, child := range properties {
			if err := validateSchemaDefinition(child); err != nil {
				return fmt.Errorf("property %q: %w", name, err)
			}
		}
	}
	if rawRequired, ok := schema["required"]; ok {
		if !hasObject {
			return errors.New("required requires type object")
		}
		if !bytes.HasPrefix(bytes.TrimSpace(rawRequired), []byte("[")) {
			return errors.New("required must be an array of property names")
		}
		var required []string
		if err := json.Unmarshal(rawRequired, &required); err != nil {
			return errors.New("required must be an array of property names")
		}
		properties := map[string]json.RawMessage{}
		if rawProperties, ok := schema["properties"]; ok {
			_ = json.Unmarshal(rawProperties, &properties)
		}
		seen := make(map[string]bool, len(required))
		for _, name := range required {
			if seen[name] {
				return fmt.Errorf("required contains duplicate property %q", name)
			}
			seen[name] = true
			if _, ok := properties[name]; !ok {
				return fmt.Errorf("required property %q has no schema in properties", name)
			}
		}
	}
	if rawAdditional, ok := schema["additionalProperties"]; ok {
		if !hasObject {
			return errors.New("additionalProperties requires type object")
		}
		additional := string(bytes.TrimSpace(rawAdditional))
		if additional != "true" && additional != "false" {
			if err := validateSchemaDefinition(rawAdditional); err != nil {
				return fmt.Errorf("additionalProperties: %w", err)
			}
		}
	}
	if rawItems, ok := schema["items"]; ok {
		if !hasArray {
			return errors.New("items requires type array")
		}
		if err := validateSchemaDefinition(rawItems); err != nil {
			return fmt.Errorf("items: %w", err)
		}
	} else if hasArray {
		return errors.New("array schema requires items")
	}
	return nil
}

func schemaTypes(schema map[string]json.RawMessage) ([]string, error) {
	rawType, ok := schema["type"]
	if !ok {
		return nil, errors.New("schema requires type")
	}
	var one string
	if json.Unmarshal(rawType, &one) == nil {
		if !supportedSchemaType(one) {
			return nil, fmt.Errorf("unsupported type %q", one)
		}
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(rawType, &many); err != nil || len(many) == 0 {
		return nil, errors.New("type must be a supported type name or non-empty array of type names")
	}
	seen := map[string]bool{}
	for _, typ := range many {
		if !supportedSchemaType(typ) {
			return nil, fmt.Errorf("unsupported type %q", typ)
		}
		if seen[typ] {
			return nil, fmt.Errorf("duplicate type %q", typ)
		}
		seen[typ] = true
	}
	return many, nil
}

func supportedSchemaType(typ string) bool {
	switch typ {
	case "object", "array", "string", "number", "integer", "boolean", "null":
		return true
	default:
		return false
	}
}

func validateJSONValue(raw, rawSchema json.RawMessage) error {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("invalid JSON value: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("JSON value contains trailing data")
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		return errors.New("invalid schema")
	}
	return validateSchemaValue(value, schema, "$ ")
}

func validateSchemaValue(value any, schema map[string]json.RawMessage, path string) error {
	types, err := schemaTypes(schema)
	if err != nil {
		return err
	}
	actual := jsonValueType(value)
	allowed := false
	for _, typ := range types {
		if typ == actual || (typ == "number" && actual == "integer") {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("%s has type %s; schema allows %s", strings.TrimSpace(path), actual, strings.Join(types, " or "))
	}
	if object, ok := value.(map[string]any); ok {
		properties := map[string]json.RawMessage{}
		if raw, exists := schema["properties"]; exists {
			if err := json.Unmarshal(raw, &properties); err != nil {
				return fmt.Errorf("%s has invalid properties schema", strings.TrimSpace(path))
			}
		}
		if raw, exists := schema["required"]; exists {
			var required []string
			if err := json.Unmarshal(raw, &required); err != nil {
				return fmt.Errorf("%s has invalid required schema", strings.TrimSpace(path))
			}
			for _, name := range required {
				if _, exists := object[name]; !exists {
					return fmt.Errorf("%s is missing required property %q", strings.TrimSpace(path), name)
				}
			}
		}
		var additionalSchema map[string]json.RawMessage
		additionalAllowed := true
		if raw, exists := schema["additionalProperties"]; exists {
			additional := string(bytes.TrimSpace(raw))
			if additional == "true" || additional == "false" {
				_ = json.Unmarshal(raw, &additionalAllowed)
			} else {
				additionalAllowed = true
				if err := json.Unmarshal(raw, &additionalSchema); err != nil {
					return fmt.Errorf("%s has invalid additionalProperties schema", strings.TrimSpace(path))
				}
			}
		}
		for name, childValue := range object {
			childSchema, known := properties[name]
			if !known {
				if len(additionalSchema) > 0 {
					if err := validateSchemaValue(childValue, additionalSchema, path+"."+name); err != nil {
						return err
					}
					continue
				}
				if !additionalAllowed {
					return fmt.Errorf("%s contains additional property %q", strings.TrimSpace(path), name)
				}
				continue
			}
			var child map[string]json.RawMessage
			if err := json.Unmarshal(childSchema, &child); err != nil {
				return fmt.Errorf("%s.%s has invalid schema", strings.TrimSpace(path), name)
			}
			if err := validateSchemaValue(childValue, child, path+"."+name); err != nil {
				return err
			}
		}
	}
	if array, ok := value.([]any); ok {
		var rawItems json.RawMessage
		if err := json.Unmarshal(schema["items"], &rawItems); err != nil || len(rawItems) == 0 {
			return fmt.Errorf("%s has no valid items schema", strings.TrimSpace(path))
		}
		var itemSchema map[string]json.RawMessage
		if err := json.Unmarshal(rawItems, &itemSchema); err != nil {
			return fmt.Errorf("%s has invalid items schema", strings.TrimSpace(path))
		}
		for i, item := range array {
			if err := validateSchemaValue(item, itemSchema, fmt.Sprintf("%s[%d]", strings.TrimSpace(path), i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func jsonValueType(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		f, err := typed.Float64()
		if err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) && math.Trunc(f) == f {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "unknown"
	}
}
