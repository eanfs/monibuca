package storage

import (
	"reflect"
	"strings"
)

// PropertyDef 属性定义
type PropertyDef struct {
	Type        string   `json:"type"`                  // string, number, boolean, object, array
	Default     any      `json:"default,omitempty"`     // 默认值
	Description string   `json:"desc,omitempty"`        // 描述
	Enum        []string `json:"enum,omitempty"`        // 枚举值
	Required    bool     `json:"required,omitempty"`    // 是否必填
	Min         *float64 `json:"min,omitempty"`         // 最小值（number类型）
	Max         *float64 `json:"max,omitempty"`         // 最大值（number类型）
	Pattern     string   `json:"pattern,omitempty"`     // 正则模式（string类型）
	Properties  Schema   `json:"properties,omitempty"`  // 嵌套对象的属性
}

// Schema 配置 Schema
type Schema map[string]PropertyDef

// StorageSchema 存储类型 Schema
type StorageSchema struct {
	Type        string `json:"type"`        // 存储类型标识
	Name        string `json:"name"`        // 显示名称
	Description string `json:"description"` // 描述
	Properties  Schema `json:"properties"`  // 配置属性
}

// SchemaRegistry 存储类型 Schema 注册表
var SchemaRegistry = make(map[string]StorageSchema)

// RegisterSchema 注册存储类型 Schema
func RegisterSchema(schema StorageSchema) {
	SchemaRegistry[schema.Type] = schema
}

// GetSchemas 获取所有已注册的存储类型 Schema
func GetSchemas() map[string]StorageSchema {
	return SchemaRegistry
}

// GetSchema 获取指定存储类型的 Schema
func GetSchema(storageType string) (StorageSchema, bool) {
	schema, ok := SchemaRegistry[storageType]
	return schema, ok
}

// GenerateSchemaFromStruct 从结构体自动生成 Schema
// 支持 json/yaml tag 作为字段名，desc tag 作为描述，default tag 作为默认值
func GenerateSchemaFromStruct(v any) Schema {
	schema := make(Schema)
	t := reflect.TypeOf(v)
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return schema
	}

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)

		// 获取字段名（优先使用 json tag，其次 yaml tag）
		fieldName := getFieldName(field)
		if fieldName == "" || fieldName == "-" {
			continue
		}

		prop := PropertyDef{
			Type:        getFieldType(field.Type),
			Description: field.Tag.Get("desc"),
		}

		// 解析 default tag
		if defaultVal := field.Tag.Get("default"); defaultVal != "" {
			prop.Default = defaultVal
		}

		// 解析 enum tag
		if enumVal := field.Tag.Get("enum"); enumVal != "" {
			prop.Enum = strings.Split(enumVal, ",")
		}

		// 判断是否必填（通过检查是否有 required tag 或字段是否为指针类型）
		if field.Tag.Get("required") == "true" {
			prop.Required = true
		}

		// 处理嵌套结构体
		if field.Type.Kind() == reflect.Struct && field.Type.String() != "time.Duration" {
			prop.Properties = GenerateSchemaFromStruct(reflect.New(field.Type).Interface())
		}

		schema[fieldName] = prop
	}

	return schema
}

// getFieldName 获取字段名
func getFieldName(field reflect.StructField) string {
	// 优先使用 json tag
	if jsonTag := field.Tag.Get("json"); jsonTag != "" {
		parts := strings.Split(jsonTag, ",")
		return parts[0]
	}
	// 其次使用 yaml tag
	if yamlTag := field.Tag.Get("yaml"); yamlTag != "" {
		parts := strings.Split(yamlTag, ",")
		return parts[0]
	}
	// 最后使用字段名的小写形式
	return strings.ToLower(field.Name)
}

// getFieldType 获取字段类型
func getFieldType(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Bool:
		return "boolean"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Struct:
		// 特殊处理 time.Duration
		if t.String() == "time.Duration" {
			return "string" // Duration 在 YAML 中通常表示为字符串如 "30s"
		}
		return "object"
	case reflect.Ptr:
		return getFieldType(t.Elem())
	default:
		return "string"
	}
}

func init() {
	// 注册 local 存储类型 Schema
	RegisterSchema(StorageSchema{
		Type:        "local",
		Name:        "本地存储",
		Description: "将文件存储到本地磁盘",
		Properties:  GenerateSchemaFromStruct(LocalStorageConfig{}),
	})
}
