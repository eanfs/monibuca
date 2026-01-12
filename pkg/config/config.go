/*
Package config provides a flexible, multi-source configuration system with priority-based value resolution.

## Overview

The config package implements a hierarchical configuration system that allows values to be set from
multiple sources with a defined priority order. This enables powerful features like:
- Environment variable overrides
- Dynamic runtime modifications
- Global and per-instance defaults
- Type-safe configuration using Go structs

## Configuration Priority

The system resolves values using the following priority order (highest to lowest):
 1. Modify  - Dynamic runtime modifications
 2. Env     - Environment variables
 3. File    - Values from config file
 4. defaultYaml - Embedded default YAML configs
 5. Global  - Global/shared configuration
 6. Default - Struct tag defaults or zero values

## Core Workflow

The configuration resolution follows a 5-step initialization process:

### Step 1: Parse
- Initialize the configuration tree from Go struct definitions
- Apply default values using struct tags
- Build the property map for all exported fields
- Set up environment variable prefixes

### Step 2: ParseGlobal
- Apply global/shared configuration values
- Useful for settings that should be consistent across instances

### Step 3: ParseDefaultYaml
- Load embedded default YAML configurations
- Provides sensible defaults without hardcoding in Go

### Step 4: ParseUserFile
- Read and apply user-provided configuration files
- Normalizes key names (removes hyphens, underscores, lowercases)
- Handles both struct mappings and single-value assignments

### Step 5: ParseModifyFile
- Apply dynamic runtime modifications
- Tracks changes separately for API purposes
- Automatically cleans up empty/unchanged values

## Key Features

### Type Conversion
The unmarshal function handles automatic conversion between different types:
- Basic types (int, string, bool, etc.)
- Duration strings with unit validation
- Regexp patterns
- Nested structs (with special handling for single non-struct values)
- Pointers, maps, slices, and arrays
- Fallback to YAML marshaling for unknown types

### Special Behaviors
- Single non-struct values are automatically assigned to the first field of struct types
- Key names are normalized (lowercase, remove hyphens/underscores)
- Environment variables use underscore-separated uppercase prefixes
- The "plugin" field is always skipped during parsing
- Fields with yaml:"-" tag are ignored

## Usage Example

```go

	type Config struct {
	    Host    string `yaml:"host" default:"localhost"`
	    Port    int    `yaml:"port" default:"8080"`
	    Timeout time.Duration `yaml:"timeout" default:"30s"`
	}

cfg := &Config{}
var c Config
c.Parse(cfg)
// Load from various sources...
config := c.GetValue().(*Config)
```

## API Structure

The main types and functions:
- Config: Core configuration node with value priority tracking
- Parse: Initialize configuration from struct
- ParseGlobal/ParseDefaultYaml/ParseUserFile/ParseModifyFile: Load from sources
- GetValue/GetMap: Retrieve resolved values
- MarshalJSON: Serialize configuration for API responses
*/
package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/mcuadros/go-defaults"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Ptr     reflect.Value // Points to config struct value, priority: Modify > Env > File > defaultYaml > Global > Default
	Modify  any           // Dynamic modified value
	Env     any           // Value from environment variable
	File    any           // Value from config file
	Global  *Config       // Value from global config (pointer type)
	Default any           // Default value
	Enum    []struct {
		Label string `json:"label"`
		Value any    `json:"value"`
	}
	name     string // Lowercase key name
	propsMap map[string]*Config
	props    []*Config
	tag      reflect.StructTag
}

var (
	durationType = reflect.TypeOf(time.Duration(0))
	regexpType   = reflect.TypeOf(Regexp{})
	basicTypes   = []reflect.Kind{
		reflect.Bool,
		reflect.Int,
		reflect.Int8,
		reflect.Int16,
		reflect.Int32,
		reflect.Int64,
		reflect.Uint,
		reflect.Uint8,
		reflect.Uint16,
		reflect.Uint32,
		reflect.Uint64,
		reflect.Float32,
		reflect.Float64,
		reflect.String,
	}
)

func (config *Config) Range(f func(key string, value Config)) {
	if m, ok := config.GetValue().(map[string]Config); ok {
		for k, v := range m {
			f(k, v)
		}
	}
}

func (config *Config) IsMap() bool {
	_, ok := config.GetValue().(map[string]Config)
	return ok
}

func (config *Config) Get(key string) (v *Config) {
	if config.propsMap == nil {
		config.propsMap = make(map[string]*Config)
	}
	key = strings.ToLower(key)
	if v, ok := config.propsMap[key]; ok {
		return v
	} else {
		v = &Config{
			name: key,
		}
		config.propsMap[key] = v
		config.props = append(config.props, v)
		return v
	}
}

func (config *Config) Has(key string) (ok bool) {
	if config.propsMap == nil {
		return false
	}
	_, ok = config.propsMap[strings.ToLower(key)]
	return ok
}

func (config *Config) MarshalJSON() ([]byte, error) {
	if config.propsMap == nil {
		return json.Marshal(config.GetValue())
	}
	return json.Marshal(config.propsMap)
}

func (config *Config) GetValue() any {
	return config.Ptr.Interface()
}

// GetProps 返回子配置列表
func (config *Config) GetProps() []*Config {
	return config.props
}

// IsDefaultValue 检查值是否等于默认值
// 对于复杂类型（切片、map、结构体），空值视为默认值
func (config *Config) IsDefaultValue(value any) bool {
	// 复杂类型的空值视为默认值
	if isEmptyValue(value) {
		return true
	}
	if config.Default == nil {
		return false
	}
	return equal(config.Default, value)
}

// IsGlobalValue 检查值是否等于全局配置值
func (config *Config) IsGlobalValue(value any) bool {
	if config.Global == nil {
		return false
	}
	return equal(config.Global.GetValue(), value)
}

// isEmptyValue 检查值是否为空值（针对复杂类型）
func isEmptyValue(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		return rv.Len() == 0
	case reflect.Map:
		return rv.Len() == 0
	case reflect.Struct:
		// 结构体：检查是否所有字段都是零值
		if rv.Type() == regexpType {
			return rv.Interface().(Regexp).Regexp == nil || rv.Interface().(Regexp).String() == ""
		}
		return rv.IsZero()
	case reflect.Ptr:
		return rv.IsNil()
	}
	return false
}

// Parse step 1: Read default values from config struct
func (config *Config) Parse(s any, prefix ...string) {
	var t reflect.Type
	var v reflect.Value
	if vv, ok := s.(reflect.Value); ok {
		t, v = vv.Type(), vv
	} else {
		t, v = reflect.TypeOf(s), reflect.ValueOf(s)
	}
	if t.Kind() == reflect.Pointer {
		t, v = t.Elem(), v.Elem()
	}
	isStruct := t.Kind() == reflect.Struct && t != regexpType
	if isStruct {
		defaults.SetDefaults(v.Addr().Interface())
	}
	config.Ptr = v
	if !v.IsValid() {
		fmt.Println("parse to ", prefix, config.name, s, "is not valid")
		return
	}
	if l := len(prefix); l > 0 { // Read environment variables
		_, isUnmarshaler := v.Addr().Interface().(yaml.Unmarshaler)
		tag := config.tag.Get("default")
		if tag != "" && isUnmarshaler {
			v.Set(config.assign(tag))
		}
		if envValue := os.Getenv(strings.Join(prefix, "_")); envValue != "" {
			v.Set(config.assign(envValue))
			config.Env = v.Interface()
		}
	}
	config.Default = v.Interface()
	if isStruct {
		for i, j := 0, t.NumField(); i < j; i++ {
			ft, fv := t.Field(i), v.Field(i)

			if !ft.IsExported() {
				continue
			}
			name := strings.ToLower(ft.Name)
			if name == "plugin" || strings.HasPrefix(name, "unimplemented") {
				continue // Skip plugin field and unimplemented fields
			}
			if tag := ft.Tag.Get("yaml"); tag != "" {
				if tag == "-" {
					continue // Skip field if tag is "-"
				}
				name, _, _ = strings.Cut(tag, ",") // Use yaml tag name, ignore options
			}
			prop := config.Get(name)

			prop.tag = ft.Tag
			if len(prefix) > 0 {
				prop.Parse(fv, append(prefix, strings.ToUpper(ft.Name))...) // Recursive parse with env prefix
			} else {
				prop.Parse(fv)
			}
			for _, kv := range strings.Split(ft.Tag.Get("enum"), ",") { // Parse enum options from tag
				kvs := strings.Split(kv, ":")
				if len(kvs) != 2 {
					continue
				}
				var tmp struct {
					Value any
				}
				yaml.Unmarshal([]byte(fmt.Sprintf("value: %s", strings.TrimSpace(kvs[0]))), &tmp)
				prop.Enum = append(prop.Enum, struct {
					Label string `json:"label"`
					Value any    `json:"value"`
				}{
					Label: strings.TrimSpace(kvs[1]),
					Value: tmp.Value,
				})
			}
		}
	}
}

// ParseGlobal step 2: Read global config
func (config *Config) ParseGlobal(g *Config) {
	config.Global = g
	if config.propsMap != nil {
		for k, v := range config.propsMap {
			v.ParseGlobal(g.Get(k))
		}
	} else {
		config.Ptr.Set(g.Ptr) // If no sub-properties, copy value directly
	}
}

// ParseDefaultYaml step 3: Read embedded default config
func (config *Config) ParseDefaultYaml(defaultYaml map[string]any) {
	if defaultYaml == nil {
		return
	}
	for k, v := range defaultYaml {
		if config.Has(k) {
			if prop := config.Get(k); prop.props != nil {
				if v != nil {
					prop.ParseDefaultYaml(v.(map[string]any))
				}
			} else {
				dv := prop.assign(v)
				prop.Default = dv.Interface()
				if prop.Env == nil { // Only set if no env var override
					prop.Ptr.Set(dv)
				}
			}
		}
	}
}

// ParseFile step 4: Read user config file
func (config *Config) ParseUserFile(conf map[string]any) {
	if conf == nil {
		return
	}
	config.File = conf
	for k, v := range conf {
		k = strings.ReplaceAll(k, "-", "") // Normalize key name: remove hyphens
		k = strings.ReplaceAll(k, "_", "") // Normalize key name: remove underscores
		k = strings.ToLower(k)
		if config.Has(k) {
			if prop := config.Get(k); prop.props != nil {
				if v != nil {
					switch vv := v.(type) {
					case map[string]any:
						prop.ParseUserFile(vv)
					default:
						// If the value is not a map (single non-struct value), assign it to the first field
						// Use unmarshal to handle type conversion properly
						fv := unmarshal(prop.props[0].Ptr.Type(), v)
						if fv.IsValid() {
							prop.props[0].Ptr.Set(fv)
						}
					}
				}
			} else {
				fv := prop.assign(v)
				if fv.IsValid() {
					prop.File = fv.Interface()
					if prop.Env == nil { // Only set if no env var override
						prop.Ptr.Set(fv)
					}
				} else {
					// Continue with invalid field
					slog.Error("Attempted to access invalid field during config parsing: %s", v)
				}
			}
		}
	}
}

// ParseModifyFile step 5: Read dynamic modified config
func (config *Config) ParseModifyFile(conf map[string]any) {
	if conf == nil {
		return
	}
	config.Modify = conf
	for k, v := range conf {
		if config.Has(k) {
			if prop := config.Get(k); prop.props != nil {
				if v != nil {
					vmap := v.(map[string]any)
					prop.ParseModifyFile(vmap)
					if len(vmap) == 0 { // Remove empty map
						delete(conf, k)
					}
				}
			} else {
				mv := prop.assign(v)
				v = mv.Interface()
				vwm := prop.valueWithoutModify() // Get value without modify
				if equal(vwm, v) {               // No change, remove from modify
					delete(conf, k)
					if prop.Modify != nil {
						prop.Modify = nil
						prop.Ptr.Set(reflect.ValueOf(vwm))
					}
					continue
				}
				prop.Modify = v
				prop.Ptr.Set(mv)
			}
		}
	}
	if len(conf) == 0 { // Clear modify if empty
		config.Modify = nil
	}
}

func (config *Config) valueWithoutModify() any {
	// Return value with priority: Env > File > Global > Default (excluding Modify)
	if config.Env != nil {
		return config.Env
	}
	if config.File != nil {
		return config.File
	}
	if config.Global != nil {
		return config.Global.GetValue()
	}
	return config.Default
}

func equal(vwm, v any) bool {
	switch ft := reflect.TypeOf(vwm); ft {
	case regexpType:
		return vwm.(Regexp).String() == v.(Regexp).String()
	default:
		switch ft.Kind() {
		case reflect.Slice, reflect.Array, reflect.Map:
			return reflect.DeepEqual(vwm, v)
		}
		return vwm == v
	}
}

func (config *Config) GetMap() map[string]any {
	// Convert config tree to map representation
	m := make(map[string]any)
	for k, v := range config.propsMap {
		if v.props != nil { // Has sub-properties
			if vv := v.GetMap(); vv != nil {
				m[k] = vv
			}
		} else if v.GetValue() != nil { // Leaf value
			m[k] = v.GetValue()
		}
	}
	if len(m) > 0 {
		return m
	}
	return nil
}

var regexPureNumber = regexp.MustCompile(`^\d+$`)

func unmarshal(ft reflect.Type, v any) (target reflect.Value) {
	source := reflect.ValueOf(v)
	// Fast path: directly return if both are basic types
	for _, t := range basicTypes {
		if source.Kind() == t && ft.Kind() == t {
			return source
		}
	}
	switch ft {
	case durationType:
		target = reflect.New(ft).Elem()
		if source.Type() == durationType {
			return source
		} else if source.IsZero() || !source.IsValid() {
			target.SetInt(0)
		} else {
			timeStr := source.String()
			// Parse duration string, but reject pure numbers (must have unit)
			if d, err := time.ParseDuration(timeStr); err == nil && !regexPureNumber.MatchString(timeStr) {
				target.SetInt(int64(d))
			} else {
				slog.Error("invalid duration value please add unit (s,m,h,d)，eg: 100ms, 10s, 4m, 1h", "value", timeStr)
				os.Exit(1)
			}
		}
	case regexpType:
		target = reflect.New(ft).Elem()
		regexpStr := source.String()
		target.Set(reflect.ValueOf(Regexp{regexp.MustCompile(regexpStr)}))
	default:
		switch ft.Kind() {
		case reflect.Pointer:
			return unmarshal(ft.Elem(), v).Addr() // Recurse to element type
		case reflect.Struct:
			newStruct := reflect.New(ft)
			defaults.SetDefaults(newStruct.Interface())
			if value, ok := v.(map[string]any); ok {
				// If the value is a map, unmarshal each field by matching keys
				for i := 0; i < ft.NumField(); i++ {
					key := strings.ToLower(ft.Field(i).Name)
					if vv, ok := value[key]; ok {
						newStruct.Elem().Field(i).Set(unmarshal(ft.Field(i).Type, vv))
					}
				}
			} else {
				// If the value is not a map (single non-struct value), assign it to the first field
				newStruct.Elem().Field(0).Set(unmarshal(ft.Field(0).Type, v))
			}
			return newStruct.Elem()
		case reflect.Map:
			if v != nil {
				target = reflect.MakeMap(ft)
				for k, v := range v.(map[string]any) {
					// Unmarshal key and value recursively
					target.SetMapIndex(unmarshal(ft.Key(), k), unmarshal(ft.Elem(), v))
				}
			}
		case reflect.Slice:
			if v != nil {
				s := v.([]any)
				target = reflect.MakeSlice(ft, len(s), len(s))
				for i, v := range s {
					target.Index(i).Set(unmarshal(ft.Elem(), v)) // Unmarshal each element
				}
			}
		default:
			if v != nil {
				// For unknown types, use YAML marshal/unmarshal as fallback
				var out []byte
				var err error
				if vv, ok := v.(string); ok {
					out = []byte(fmt.Sprintf("%s: %s", "value", vv))
				} else {
					out, err = yaml.Marshal(map[string]any{"value": v})
					if err != nil {
						panic(err)
					}
				}
				// Create temporary struct with single Value field
				tmpValue := reflect.New(reflect.StructOf([]reflect.StructField{
					{
						Name: "Value",
						Type: ft,
					},
				}))
				err = yaml.Unmarshal(out, tmpValue.Interface())
				if err != nil {
					panic(err)
				}
				return tmpValue.Elem().Field(0)
			}
		}
	}
	return
}

func (config *Config) assign(v any) reflect.Value {
	// Convert value to the same type as Ptr
	return unmarshal(config.Ptr.Type(), v)
}

func Parse(target any, conf map[string]any) {
	var c Config
	c.Parse(target)
	c.ParseModifyFile(conf)
}
