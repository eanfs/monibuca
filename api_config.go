package m7s

import (
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func getIndent(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

func addCommentsToYAML(yamlData []byte) []byte {
	lines := strings.Split(string(yamlData), "\n")
	var result strings.Builder
	var commentBuffer []string
	var keyLineBuffer string
	var keyLineIndent int
	inMultilineValue := false

	for _, line := range lines {
		trimmedLine := strings.TrimSpace(line)
		indent := getIndent(line)

		if strings.HasPrefix(trimmedLine, "_description:") {
			description := strings.TrimSpace(strings.TrimPrefix(trimmedLine, "_description:"))
			commentBuffer = append(commentBuffer, "# "+description)
		} else if strings.HasPrefix(trimmedLine, "_enum:") {
			enum := strings.TrimSpace(strings.TrimPrefix(trimmedLine, "_enum:"))
			commentBuffer = append(commentBuffer, "# 可选值: "+enum)
		} else if strings.HasPrefix(trimmedLine, "_value:") {
			valueStr := strings.TrimSpace(strings.TrimPrefix(trimmedLine, "_value:"))
			if valueStr != "" && valueStr != "{}" && valueStr != "[]" {
				// Single line value
				result.WriteString(strings.Repeat(" ", keyLineIndent))
				result.WriteString(keyLineBuffer)
				result.WriteString(": ")
				result.WriteString(valueStr)
				if len(commentBuffer) > 0 {
					result.WriteString(" ")
					for j, c := range commentBuffer {
						c = strings.TrimSpace(strings.TrimPrefix(c, "#"))
						result.WriteString("# " + c)
						if j < len(commentBuffer)-1 {
							result.WriteString(" ")
						}
					}
				}
				result.WriteString("\n")
			} else {
				// Multi-line value (struct/map)
				for _, comment := range commentBuffer {
					result.WriteString(strings.Repeat(" ", keyLineIndent))
					result.WriteString(comment)
					result.WriteString("\n")
				}
				result.WriteString(strings.Repeat(" ", keyLineIndent))
				result.WriteString(keyLineBuffer)
				result.WriteString(":")
				result.WriteString("\n")
				inMultilineValue = true
			}
			commentBuffer = nil
			keyLineBuffer = ""
			keyLineIndent = 0
		} else if strings.Contains(trimmedLine, ":") {
			// This is a key line
			if keyLineBuffer != "" { // flush previous key line
				result.WriteString(strings.Repeat(" ", keyLineIndent) + keyLineBuffer + ":\n")
			}
			inMultilineValue = false
			keyLineBuffer = strings.TrimSuffix(trimmedLine, ":")
			keyLineIndent = indent
		} else if inMultilineValue {
			// These are the lines of a multiline value
			if trimmedLine != "" {
				result.WriteString(line + "\n")
			}
		}
	}
	if keyLineBuffer != "" {
		result.WriteString(strings.Repeat(" ", keyLineIndent) + keyLineBuffer + ":\n")
	}

	// Final cleanup to remove empty lines and special keys
	finalOutput := []string{}
	for _, line := range strings.Split(result.String(), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "_") {
			continue
		}
		finalOutput = append(finalOutput, line)
	}

	return []byte(strings.Join(finalOutput, "\n"))
}

func (s *Server) api_Config_YAML_All(rw http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	filterName := query.Get("name")
	shouldMergeCommon := query.Get("common") != "false"

	configSections := []struct {
		name string
		data any
	}{}

	// 1. Get common config if it needs to be merged.
	var commonConfig map[string]any
	if shouldMergeCommon {
		if c, ok := extractStructConfig(reflect.ValueOf(s.Plugin.GetCommonConf())).(map[string]any); ok {
			commonConfig = c
		}
	}

	// 2. Process global config.
	if filterName == "" || filterName == "global" {
		if globalConf, ok := extractStructConfig(reflect.ValueOf(s.ServerConfig)).(map[string]any); ok {
			if shouldMergeCommon && commonConfig != nil {
				mergedConf := make(map[string]any)
				for k, v := range commonConfig {
					mergedConf[k] = v
				}
				for k, v := range globalConf {
					mergedConf[k] = v // Global overrides common
				}
				configSections = append(configSections, struct {
					name string
					data any
				}{"global", mergedConf})
			} else {
				configSections = append(configSections, struct {
					name string
					data any
				}{"global", globalConf})
			}
		}
	}

	// 3. Process plugin configs.
	for _, meta := range plugins {
		if filterName != "" && !strings.EqualFold(meta.Name, filterName) {
			continue
		}
		name := strings.ToLower(meta.Name)
		configType := meta.Type
		if configType.Kind() == reflect.Ptr {
			configType = configType.Elem()
		}

		if pluginConf, ok := extractStructConfig(reflect.New(configType)).(map[string]any); ok {
			pluginConf["enable"] = map[string]any{
				"_value":       true,
				"_description": "在global配置disableall时能启用特定插件",
			}
			if shouldMergeCommon && commonConfig != nil {
				mergedConf := make(map[string]any)
				for k, v := range commonConfig {
					mergedConf[k] = v
				}
				for k, v := range pluginConf {
					mergedConf[k] = v // Plugin overrides common
				}
				configSections = append(configSections, struct {
					name string
					data any
				}{name, mergedConf})
			} else {
				configSections = append(configSections, struct {
					name string
					data any
				}{name, pluginConf})
			}
		}
	}

	// 4. Serialize each section and combine.
	var yamlParts []string
	for _, section := range configSections {
		if section.data == nil {
			continue
		}
		partMap := map[string]any{section.name: section.data}
		partYAML, err := yaml.Marshal(partMap)
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		yamlParts = append(yamlParts, string(partYAML))
	}

	finalYAML := strings.Join(yamlParts, "")

	rw.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	rw.Write(addCommentsToYAML([]byte(finalYAML)))
}

func extractStructConfig(v reflect.Value) any {
	if v.Kind() == reflect.Ptr {
		if v.IsNil() {
			return nil
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return nil
	}
	m := make(map[string]any)
	for i := 0; i < v.NumField(); i++ {
		field := v.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		// Filter out Plugin and UnimplementedApiServer
		fieldType := field.Type
		if fieldType.Kind() == reflect.Ptr {
			fieldType = fieldType.Elem()
		}
		if fieldType.Name() == "Plugin" || fieldType.Name() == "UnimplementedApiServer" {
			continue
		}
		yamlTag := field.Tag.Get("yaml")
		if yamlTag == "-" {
			continue
		}
		fieldName := strings.Split(yamlTag, ",")[0]
		if fieldName == "" {
			fieldName = strings.ToLower(field.Name)
		}
		m[fieldName] = extractFieldConfig(field, v.Field(i))
	}
	return m
}

func extractFieldConfig(field reflect.StructField, value reflect.Value) any {
	result := make(map[string]any)
	description := field.Tag.Get("desc")
	enum := field.Tag.Get("enum")
	if description != "" {
		result["_description"] = description
	}
	if enum != "" {
		result["_enum"] = enum
	}

	kind := value.Kind()
	if kind == reflect.Ptr {
		if value.IsNil() {
			value = reflect.New(value.Type().Elem())
		}
		value = value.Elem()
		kind = value.Kind()
	}

	switch kind {
	case reflect.Struct:
		if dur, ok := value.Interface().(time.Duration); ok {
			result["_value"] = extractDurationConfig(field, dur)
		} else {
			result["_value"] = extractStructConfig(value)
		}
	case reflect.Map, reflect.Slice:
		if value.IsNil() {
			result["_value"] = make(map[string]any)
			if kind == reflect.Slice {
				result["_value"] = make([]any, 0)
			}
		} else {
			result["_value"] = value.Interface()
		}
	default:
		result["_value"] = extractBasicTypeConfig(field, value)
	}

	if description == "" && enum == "" {
		return result["_value"]
	}

	return result
}

func extractBasicTypeConfig(field reflect.StructField, value reflect.Value) any {
	if value.IsZero() {
		if defaultValue := field.Tag.Get("default"); defaultValue != "" {
			return parseDefaultValue(defaultValue, field.Type)
		}
	}
	return value.Interface()
}

func extractDurationConfig(field reflect.StructField, value time.Duration) any {
	if value == 0 {
		if defaultValue := field.Tag.Get("default"); defaultValue != "" {
			return defaultValue
		}
	}
	return value.String()
}

func parseDefaultValue(defaultValue string, t reflect.Type) any {
	switch t.Kind() {
	case reflect.String:
		return defaultValue
	case reflect.Bool:
		return defaultValue == "true"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v, err := strconv.ParseInt(defaultValue, 10, 64); err == nil {
			return v
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if v, err := strconv.ParseUint(defaultValue, 10, 64); err == nil {
			return v
		}
	case reflect.Float32, reflect.Float64:
		if v, err := strconv.ParseFloat(defaultValue, 64); err == nil {
			return v
		}
	}
	return defaultValue
}
