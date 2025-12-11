package config

import (
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"time"

	"m7s.live/v5/pkg/util"
)

type Property struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Enum        []struct {
		Label string `json:"label"`
		Value any    `json:"value"`
	} `json:"enum,omitempty"`
	Items          any            `json:"items,omitempty"`
	Properties     map[string]any `json:"properties,omitempty"`
	Default        any            `json:"default,omitempty"`
	Decorator      string         `json:"x-decorator"`
	DecoratorProps map[string]any `json:"x-decorator-props,omitempty"`
	Component      string         `json:"x-component"`
	ComponentProps map[string]any `json:"x-component-props,omitempty"`
	Index          int            `json:"x-index"`
	// 新增字段：用于前端显示当前值和是否使用默认值
	CurrentValue   any    `json:"x-current-value,omitempty"`   // 当前实际使用的值
	IsDefault      bool   `json:"x-is-default"`                // 是否正在使用默认值
	DefaultValue   any    `json:"x-default-value,omitempty"`   // 默认值
	ValueSource    string `json:"x-value-source,omitempty"`    // 值来源: default, global, file, env, modify
	// 复杂类型显示控制
	ComplexType    string `json:"x-complex-type,omitempty"`    // 复杂类型标识: small-object, large-object, array, map
	FieldCount     int    `json:"x-field-count,omitempty"`     // 字段数量（用于判断是否折叠）
}

type Card struct {
	Type           string         `json:"type"`
	Properties     map[string]any `json:"properties,omitempty"`
	Component      string         `json:"x-component"`
	ComponentProps map[string]any `json:"x-component-props,omitempty"`
	Index          int            `json:"x-index"`
}

// CollapseItem 用于 ArrayCollapse 的 items schema
// ArrayCollapse 组件期望 items['x-component-props'].header 存在
type CollapseItem struct {
	Type           string         `json:"type"`
	Component      string         `json:"x-component"`
	ComponentProps map[string]any `json:"x-component-props,omitempty"`
	Properties     map[string]any `json:"properties,omitempty"`
}

type Object struct {
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
}

// hasNestedComplexType 检查结构体是否包含嵌套的复杂类型（map、slice、struct）
func hasNestedComplexType(t reflect.Type) bool {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() || field.Tag.Get("yaml") == "-" || field.Anonymous {
			continue
		}
		fieldType := field.Type
		if fieldType.Kind() == reflect.Ptr {
			fieldType = fieldType.Elem()
		}
		switch fieldType.Kind() {
		case reflect.Map, reflect.Slice:
			return true
		case reflect.Struct:
			if fieldType != regexpType && fieldType != durationType {
				return true
			}
		}
	}
	return false
}

// buildFieldProperty 根据字段类型构建 Property
func buildFieldProperty(field reflect.StructField, index int, title string) Property {
	fieldType := field.Type
	if fieldType.Kind() == reflect.Ptr {
		fieldType = fieldType.Elem()
	}
	
	prop := Property{
		Title:     title,
		Decorator: "FormItem",
		Index:     index,
		DecoratorProps: map[string]any{
			"tooltip": field.Tag.Get("desc"),
		},
	}
	
	switch fieldType {
	case durationType:
		prop.Type = "string"
		prop.Component = "Input"
		prop.DecoratorProps["addonAfter"] = "时间,单位：s,m,h,d"
	case regexpType:
		prop.Type = "string"
		prop.Component = "Input"
		prop.DecoratorProps["addonAfter"] = "正则表达式"
	default:
		switch fieldType.Kind() {
		case reflect.Bool:
			prop.Type = "boolean"
			prop.Component = "Switch"
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
			prop.Type = "number"
			prop.Component = "InputNumber"
		default:
			prop.Type = "string"
			prop.Component = "Input"
		}
	}
	
	return prop
}

func (config *Config) schema(index int) (r any) {
	defer func() {
		err := recover()
		if err != nil {
			slog.Error(err.(error).Error())
		}
	}()
	if config.props != nil {
		r := Card{
			Type:       "void",
			Component:  "Card",
			Properties: make(map[string]any),
			Index:      index,
		}
		r.ComponentProps = map[string]any{
			"title": config.name,
		}
		for i, v := range config.props {
			if strings.HasPrefix(v.tag.Get("desc"), "废弃") {
				continue
			}
			r.Properties[v.name] = v.schema(i)
		}
		return r
	} else {
		// 计算值来源和是否使用默认值
		var valueSource string
		var isDefault bool
		if config.Modify != nil {
			valueSource = "modify"
		} else if config.Env != nil {
			valueSource = "env"
		} else if config.File != nil {
			valueSource = "file"
		} else if config.Global != nil {
			valueSource = "global"
		} else {
			valueSource = "default"
			isDefault = true
		}

		p := Property{
			Title:   config.name,
			Default: config.GetValue(),
			DecoratorProps: map[string]any{
				"tooltip": config.tag.Get("desc"),
			},
			ComponentProps: map[string]any{},
			Decorator:      "ConfigFormItem",
			Index:          index,
			// 新增字段
			CurrentValue: config.GetValue(),
			DefaultValue: config.Default,
			IsDefault:    isDefault,
			ValueSource:  valueSource,
		}
		if config.Modify != nil {
			p.Description = "已动态修改"
		} else if config.Env != nil {
			p.Description = "使用环境变量中的值"
		} else if config.File != nil {
			p.Description = "使用配置文件中的值"
		} else if config.Global != nil {
			p.Description = "已使用全局配置中的值"
		}
		p.Enum = config.Enum
		switch config.Ptr.Type() {
		case regexpType:
			p.Type = "string"
			p.Component = "Input"
			p.DecoratorProps["addonAfter"] = "正则表达式"
			str := config.GetValue().(Regexp).String()
			p.ComponentProps = map[string]any{
				"placeholder": str,
			}
			p.Default = str
			p.CurrentValue = str
			if config.Default != nil {
				p.DefaultValue = config.Default.(Regexp).String()
			}
		case durationType:
			p.Type = "string"
			p.Component = "Input"
			str := config.GetValue().(time.Duration).String()
			p.ComponentProps = map[string]any{
				"placeholder": str,
			}
			p.Default = str
			p.CurrentValue = str
			if config.Default != nil {
				p.DefaultValue = config.Default.(time.Duration).String()
			}
			p.DecoratorProps["addonAfter"] = "时间,单位：s,m,h,d，例如：100ms, 10s, 4m, 1h"
		default:
			// 检查是否是 util.Range 类型（泛型类型需要通过类型名称判断）
			typeName := config.Ptr.Type().String()
			if strings.Contains(typeName, "util.Range[") {
				p.Type = "string"
				p.Component = "Input"
				// 将 Range 转换为字符串格式 "min-max"
				str := util.RangeToString(config.Ptr)
				p.ComponentProps = map[string]any{
					"placeholder": str,
				}
				p.Default = str
				p.CurrentValue = str
				if config.Default != nil {
					p.DefaultValue = util.RangeToString(reflect.ValueOf(config.Default))
				}
				p.DecoratorProps["addonAfter"] = "范围，格式：min-max，例如：10001-20000"
				if len(p.Enum) > 0 {
					p.Component = "Radio.Group"
				}
				return p
			}
			switch config.Ptr.Kind() {
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Float32, reflect.Float64:
				p.Type = "number"
				p.Component = "InputNumber"
				p.ComponentProps = map[string]any{
					"placeholder": fmt.Sprintf("%v", config.GetValue()),
				}
			case reflect.Bool:
				p.Type = "boolean"
				p.Component = "Switch"
			case reflect.String:
				p.Type = "string"
				p.Component = "Input"
				p.ComponentProps = map[string]any{
					"placeholder": config.GetValue(),
				}
			case reflect.Slice:
				// 获取 slice 的元素类型
				elemType := config.Ptr.Type().Elem()
				// 如果元素是指针，获取指针指向的类型
				if elemType.Kind() == reflect.Ptr {
					elemType = elemType.Elem()
				}
				elemIsStruct := elemType.Kind() == reflect.Struct && elemType != regexpType
				
				if elemIsStruct {
					// 计算结构体字段数量
					fieldCount := 0
					for i := 0; i < elemType.NumField(); i++ {
						field := elemType.Field(i)
						if field.IsExported() && field.Tag.Get("yaml") != "-" && !field.Anonymous {
							fieldCount++
						}
					}
					
					// 构建 children 数据
					var children []map[string]any
					for i := 0; i < config.Ptr.Len(); i++ {
						elem := config.Ptr.Index(i)
						if elem.Kind() == reflect.Ptr {
							elem = elem.Elem()
						}
						if elem.Kind() == reflect.Struct {
							structMap := make(map[string]any)
							for j := 0; j < elem.NumField(); j++ {
								field := elemType.Field(j)
								if !field.IsExported() || field.Tag.Get("yaml") == "-" || field.Anonymous {
									continue
								}
								fieldName := strings.ToLower(field.Name)
								fieldValue := elem.Field(j)
								if fieldValue.CanInterface() {
									// 特殊处理 time.Duration 类型
									if fieldValue.Type() == durationType {
										structMap[fieldName] = fieldValue.Interface().(time.Duration).String()
									} else {
										structMap[fieldName] = fieldValue.Interface()
									}
								}
							}
							children = append(children, structMap)
						}
					}
					
					// 字段数 > 3 使用 ArrayCards（折叠卡片模式）
					if fieldCount > 3 {
						// 构建卡片内的表单字段
						cardProps := make(map[string]any)
						fieldIndex := 0
						for i := 0; i < elemType.NumField(); i++ {
							field := elemType.Field(i)
							if !field.IsExported() || field.Tag.Get("yaml") == "-" || field.Anonymous {
								continue
							}
							fieldName := strings.ToLower(field.Name)
							fieldDesc := field.Tag.Get("desc")
							if fieldDesc == "" {
								fieldDesc = field.Name
							}
							
							fieldProp := buildFieldProperty(field, fieldIndex, fieldDesc)
							cardProps[fieldName] = fieldProp
							fieldIndex++
						}
						
						p = Property{
							Type:        "array",
							Component:   "ArrayCards",
							Decorator:   "FormItem",
							Title:       config.name,
							Index:       index,
							ComplexType: "array",
							FieldCount:  fieldCount,
							Properties: map[string]any{
								"addition": map[string]any{
									"type":        "void",
									"title":       "添加",
									"x-component": "ArrayCards.Addition",
								},
							},
							Items: &Object{
								Type: "object",
								Properties: map[string]any{
									"index": Card{
										Type:      "void",
										Component: "ArrayCards.Index",
										Index:     0,
									},
									"card": Card{
										Type:      "void",
										Component: "FormLayout",
										ComponentProps: map[string]any{
											"layout":     "vertical",
											"labelCol":   6,
											"wrapperCol": 18,
										},
										Properties: cardProps,
										Index:      1,
									},
									"remove": Card{
										Type:      "void",
										Component: "ArrayCards.Remove",
										Index:     2,
									},
									"moveUp": Card{
										Type:      "void",
										Component: "ArrayCards.MoveUp",
										Index:     3,
									},
									"moveDown": Card{
										Type:      "void",
										Component: "ArrayCards.MoveDown",
										Index:     4,
									},
								},
							},
							Default: children,
						}
						return p
					}
					
					// 字段数 <= 3 使用 ArrayTable（表格模式）
					columnProps := make(map[string]any)
					colIndex := 0
					for i := 0; i < elemType.NumField(); i++ {
						field := elemType.Field(i)
						if !field.IsExported() {
							continue
						}
						// 跳过 gorm 和 yaml:"-" 标记的字段
						if yamlTag := field.Tag.Get("yaml"); yamlTag == "-" {
							continue
						}
						
						fieldName := strings.ToLower(field.Name)
						fieldDesc := field.Tag.Get("desc")
						if fieldDesc == "" {
							fieldDesc = field.Name
						}
						
						// 跳过嵌入的结构体字段（如 config.Pull, config.Record）
						if field.Anonymous {
							continue
						}
						
						// 根据字段类型生成对应的表单组件
						var fieldProp Property
						fieldType := field.Type
						if fieldType.Kind() == reflect.Ptr {
							fieldType = fieldType.Elem()
						}
						
						switch fieldType.Kind() {
						case reflect.Bool:
							fieldProp = Property{
								Type:      "boolean",
								Decorator: "FormItem",
								Component: "Switch",
								Index:     colIndex,
							}
						case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
							reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
							reflect.Float32, reflect.Float64:
							fieldProp = Property{
								Type:      "number",
								Decorator: "FormItem",
								Component: "InputNumber",
								Index:     colIndex,
							}
						default:
							// 默认使用字符串输入框
							fieldProp = Property{
								Type:      "string",
								Decorator: "FormItem",
								Component: "Input",
								Index:     colIndex,
							}
						}
						
						colName := fmt.Sprintf("c%d", colIndex)
						columnProps[colName] = Card{
							Type:      "void",
							Component: "ArrayTable.Column",
							ComponentProps: map[string]any{
								"title": fieldDesc,
							},
							Properties: map[string]any{
								fieldName: fieldProp,
							},
							Index: colIndex,
						}
						colIndex++
					}
					
					// 添加操作列
					columnProps["operator"] = Card{
						Type:      "void",
						Component: "ArrayTable.Column",
						ComponentProps: map[string]any{
							"title": "操作",
							"width": 80,
						},
						Properties: map[string]any{
							"remove": Card{
								Type:      "void",
								Component: "ArrayTable.Remove",
							},
						},
						Index: colIndex,
					}
					
					p = Property{
						Type:        "array",
						Component:   "ArrayTable",
						Decorator:   "FormItem",
						ComplexType: "array",
						FieldCount:  fieldCount,
						Properties: map[string]any{
							"addition": map[string]string{
								"type":        "void",
								"title":       "添加",
								"x-component": "ArrayTable.Addition",
							},
						},
						Index: index,
						Title: config.name,
						Items: &Object{
							Type:       "object",
							Properties: columnProps,
						},
						Default: children,
					}
					return p
				}
				
				// 元素是基本类型，使用简单的 Input
				p.Type = "array"
				p.Component = "Input"
				p.ComponentProps = map[string]any{
					"placeholder": config.GetValue(),
				}
				p.DecoratorProps["addonAfter"] = "数组，每个元素用逗号分隔"
			case reflect.Map:
				// 获取 map 的 key 类型
				keyType := config.Ptr.Type().Key()
				keyIsRegexp := keyType == regexpType
				keyComponent := "Input"
				keyAddonAfter := ""
				if keyIsRegexp {
					keyAddonAfter = "正则表达式"
				}
				
				// 获取 map 的 value 类型
				valueType := config.Ptr.Type().Elem()
				// 如果 value 是指针类型，获取其元素类型
				if valueType.Kind() == reflect.Ptr {
					valueType = valueType.Elem()
				}
				valueIsStruct := valueType.Kind() == reflect.Struct && valueType != regexpType
				valueIsMap := valueType.Kind() == reflect.Map
				
				// 计算 value 的字段数量（用于判断复杂度）
				valueFieldCount := 0
				if valueIsStruct {
					for i := 0; i < valueType.NumField(); i++ {
						field := valueType.Field(i)
						if field.IsExported() && field.Tag.Get("yaml") != "-" && !field.Anonymous {
							valueFieldCount++
						}
					}
				}
				
				// 判断是否是复杂类型（字段数 > 3 或者 value 本身是 map/slice）
				isComplexValue := valueFieldCount > 3 || valueIsMap || (valueIsStruct && hasNestedComplexType(valueType))
				
				// 构建 children 数据
				var children []map[string]any
				iter := config.Ptr.MapRange()
				for iter.Next() {
					// 将 key 转换为字符串（支持 Regexp 类型）
					var keyStr string
					if keyIsRegexp {
						keyStr = iter.Key().Interface().(Regexp).String()
					} else {
						keyStr = fmt.Sprintf("%v", iter.Key().Interface())
					}
					
					// 将 value 转换为适合表单的格式
					var valueForForm any
					val := iter.Value()
					if val.Kind() == reflect.Ptr && !val.IsNil() {
						val = val.Elem()
					}
					if valueIsStruct && val.Kind() == reflect.Struct {
						// 结构体类型，转换为 map
						structMap := make(map[string]any)
						for i := 0; i < val.NumField(); i++ {
							field := valueType.Field(i)
							if !field.IsExported() || field.Tag.Get("yaml") == "-" || field.Anonymous {
								continue
							}
							fieldName := strings.ToLower(field.Name)
							fieldValue := val.Field(i)
							if fieldValue.CanInterface() {
								// 特殊处理 time.Duration 类型
								if fieldValue.Type() == durationType {
									structMap[fieldName] = fieldValue.Interface().(time.Duration).String()
								} else {
									structMap[fieldName] = fieldValue.Interface()
								}
							}
						}
						valueForForm = structMap
					} else {
						if val.CanInterface() {
							valueForForm = val.Interface()
						}
					}
					
					children = append(children, map[string]any{
						"mkey":   keyStr,
						"mvalue": valueForForm,
					})
				}
				
				// 复杂类型使用 ArrayCollapse（折叠面板 + 数组）
				if isComplexValue {
					// 构建 value 的 schema（用于折叠面板内部）
					var valueSchema map[string]any
					if valueIsStruct {
						structProps := make(map[string]any)
						fieldIndex := 0
						for i := 0; i < valueType.NumField(); i++ {
							field := valueType.Field(i)
							if !field.IsExported() || field.Tag.Get("yaml") == "-" || field.Anonymous {
								continue
							}
							fieldName := strings.ToLower(field.Name)
							fieldDesc := field.Tag.Get("desc")
							if fieldDesc == "" {
								fieldDesc = field.Name
							}
							
							fieldProp := buildFieldProperty(field, fieldIndex+2, fieldDesc)
							structProps[fieldName] = fieldProp
							fieldIndex++
						}
						valueSchema = structProps
					}
					
					// 构建 CollapsePanel 内部的 properties
					// 包含：index, mkey, mvalue 字段, remove, moveUp, moveDown
					collapsePanelProps := map[string]any{
						"index": Card{
							Type:      "void",
							Component: "ArrayCollapse.Index",
							Index:     0,
						},
						"mkey": Property{
							Type:      "string",
							Title:     config.tag.Get("key"),
							Decorator: "FormItem",
							Component: keyComponent,
							DecoratorProps: map[string]any{
								"addonAfter": keyAddonAfter,
							},
							Index: 1,
						},
					}
					
					// 添加 mvalue 字段（如果是结构体）
					if valueIsStruct && valueSchema != nil {
						collapsePanelProps["mvalue"] = Card{
							Type:       "object",
							Component:  "FormLayout",
							Properties: valueSchema,
							ComponentProps: map[string]any{
								"layout":     "vertical",
								"labelCol":   6,
								"wrapperCol": 18,
							},
							Index: 2,
						}
					}
					
					// 添加操作按钮
					collapsePanelProps["remove"] = Card{
						Type:      "void",
						Component: "ArrayCollapse.Remove",
						Index:     100,
					}
					collapsePanelProps["moveUp"] = Card{
						Type:      "void",
						Component: "ArrayCollapse.MoveUp",
						Index:     101,
					}
					collapsePanelProps["moveDown"] = Card{
						Type:      "void",
						Component: "ArrayCollapse.MoveDown",
						Index:     102,
					}
					
					// 获取 key 的标题作为 header
					keyTitle := config.tag.Get("key")
					if keyTitle == "" {
						keyTitle = "配置项"
					}
					
					p := Property{
						Type:        "array",
						Component:   "ArrayCollapse",
						Decorator:   "FormItem",
						Title:       config.name,
						Index:       index,
						ComplexType: "map",
						FieldCount:  valueFieldCount,
						ComponentProps: map[string]any{
							"accordion": false,
						},
						Properties: map[string]any{
							"addition": map[string]any{
								"type":        "void",
								"title":       "添加",
								"x-component": "ArrayCollapse.Addition",
							},
						},
						// items 直接就是 CollapsePanel 的 schema
						// ArrayCollapse 组件期望 items['x-component-props'].header 存在
						Items: &CollapseItem{
							Type:       "object",
							Component:  "ArrayCollapse.CollapsePanel",
							Properties: collapsePanelProps,
							ComponentProps: map[string]any{
								"header": keyTitle,
							},
						},
						Default: children,
					}
					return p
				}
				
				// 简单类型继续使用 ArrayTable
				var valueColumnProps map[string]any
				if valueIsStruct {
					// value 是结构体，生成嵌套表单字段
					structProps := make(map[string]any)
					for i := 0; i < valueType.NumField(); i++ {
						field := valueType.Field(i)
						if !field.IsExported() {
							continue
						}
						fieldName := strings.ToLower(field.Name)
						fieldDesc := field.Tag.Get("desc")
						if fieldDesc == "" {
							fieldDesc = field.Name
						}
						
						// 根据字段类型生成对应的表单组件
						var fieldProp Property
						switch field.Type.Kind() {
						case reflect.Bool:
							fieldProp = Property{
								Type:      "boolean",
								Title:     fieldDesc,
								Decorator: "FormItem",
								Component: "Switch",
								Index:     i,
							}
						case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
							reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
							reflect.Float32, reflect.Float64:
							fieldProp = Property{
								Type:      "number",
								Title:     fieldDesc,
								Decorator: "FormItem",
								Component: "InputNumber",
								Index:     i,
							}
						default:
							// 默认使用字符串输入框
							fieldProp = Property{
								Type:      "string",
								Title:     fieldDesc,
								Decorator: "FormItem",
								Component: "Input",
								Index:     i,
							}
						}
						structProps[fieldName] = fieldProp
					}
					valueColumnProps = map[string]any{
						"mvalue": Card{
							Type:       "object",
							Component:  "FormLayout",
							Properties: structProps,
							ComponentProps: map[string]any{
								"layout": "vertical",
							},
						},
					}
				} else {
					// value 是基本类型
					valueColumnProps = map[string]any{
						"mvalue": Property{
							Type:      "string",
							Decorator: "FormItem",
							Component: "Input",
						},
					}
				}
				
				p := Property{
					Type:        "array",
					Component:   "ArrayTable",
					Decorator:   "FormItem",
					ComplexType: "map",
					FieldCount:  valueFieldCount,
					Properties: map[string]any{
						"addition": map[string]string{
							"type":        "void",
							"title":       "添加",
							"x-component": "ArrayTable.Addition",
						},
					},
					Index: index,
					Title: config.name,
					Items: &Object{
						Type: "object",
						Properties: map[string]any{
							"c1": Card{
								Type:      "void",
								Component: "ArrayTable.Column",
								ComponentProps: map[string]any{
									"title": config.tag.Get("key"),
									"width": 300,
								},
								Properties: map[string]any{
									"mkey": Property{
										Type:      "string",
										Decorator: "FormItem",
										Component: keyComponent,
										DecoratorProps: map[string]any{
											"addonAfter": keyAddonAfter,
										},
									},
								},
								Index: 0,
							},
							"c2": Card{
								Type:      "void",
								Component: "ArrayTable.Column",
								ComponentProps: map[string]any{
									"title": config.tag.Get("value"),
								},
								Properties: valueColumnProps,
								Index: 1,
							},
							"operator": Card{
								Type:      "void",
								Component: "ArrayTable.Column",
								ComponentProps: map[string]any{
									"title": "操作",
								},
								Properties: map[string]any{
									"remove": Card{
										Type:      "void",
										Component: "ArrayTable.Remove",
									},
								},
								Index: 2,
							},
						},
					},
					Default: children,
				}
				return p
			default:

			}
		}
		if len(p.Enum) > 0 {
			p.Component = "Radio.Group"
		}
		return p
	}
}

// buildNestedTabs 将配置项分组为嵌套 Tab：带子级的各占一个 Tab，顶级配置合并到"基础配置" Tab
func buildNestedTabs(props []*Config, tabPrefix string) map[string]any {
	tabsProps := make(map[string]any)
	
	// 分离带子级的配置和顶级配置
	var nestedProps []*Config  // 带子级的配置
	var basicProps []*Config   // 顶级配置（无子级）
	
	for _, v := range props {
		if len(v.props) > 0 {
			nestedProps = append(nestedProps, v)
		} else {
			basicProps = append(basicProps, v)
		}
	}
	
	tabIndex := 0
	
	// 如果有顶级配置，合并到"基础配置" Tab
	if len(basicProps) > 0 {
		basicTabProps := make(map[string]any)
		for i, v := range basicProps {
			basicTabProps[v.name] = v.schema(i)
		}
		tabsProps[tabPrefix+"_basic"] = Card{
			Type:      "void",
			Component: "FormTab.TabPane",
			ComponentProps: map[string]any{
				"tab": "基础配置",
			},
			Properties: basicTabProps,
			Index:      tabIndex,
		}
		tabIndex++
	}
	
	// 带子级的配置各占一个 Tab
	// 使用 object 类型而不是 void，这样每个 tab 内的字段会有唯一的路径
	// 例如：common_http.listenaddr 和 common_udp.listenaddr 不会冲突
	for _, v := range nestedProps {
		nestedTabProps := make(map[string]any)
		for i, child := range v.props {
			if strings.HasPrefix(child.tag.Get("desc"), "废弃") {
				continue
			}
			nestedTabProps[child.name] = child.schema(i)
		}
		
		// 获取 Tab 标题，优先使用 desc 标签
		tabTitle := v.name
		if desc := v.tag.Get("desc"); desc != "" {
			tabTitle = desc
		}
		
		tabsProps[tabPrefix+"_"+v.name] = Card{
			Type:      "object",  // 使用 object 类型，使字段路径唯一
			Component: "FormTab.TabPane",
			ComponentProps: map[string]any{
				"tab": tabTitle,
			},
			Properties: nestedTabProps,
			Index:      tabIndex,
		}
		tabIndex++
	}
	
	return tabsProps
}

func (config *Config) GetFormily() (r Object) {
	var fromItems = make(map[string]any)
	r.Type = "object"
	r.Properties = map[string]any{
		"layout": Card{
			Type:      "void",
			Component: "FormLayout",
			ComponentProps: map[string]any{
				"labelCol":   4,
				"wrapperCol": 20,
			},
			Properties: fromItems,
		},
	}
	
	// 分离公共配置和插件特有配置
	var commonProps []*Config
	var pluginProps []*Config
	
	// 公共配置字段名列表
	commonFields := map[string]bool{
		"publicip": true, "publicipv6": true, "loglevel": true, "enableauth": true,
		"publish": true, "subscribe": true, "http": true, "quic": true,
		"tcp": true, "udp": true, "hook": true, "pull": true, "transform": true,
		"onsub": true, "onpub": true, "db": true,
	}
	
	for _, v := range config.props {
		if strings.HasPrefix(v.tag.Get("desc"), "废弃") {
			continue
		}
		if commonFields[v.name] {
			commonProps = append(commonProps, v)
		} else {
			pluginProps = append(pluginProps, v)
		}
	}
	
	// 如果既有公共配置又有插件特有配置，使用 Tabs
	if len(commonProps) > 0 && len(pluginProps) > 0 {
		tabsProps := make(map[string]any)
		
		// 插件特有配置 Tab - 内部再用嵌套 Tab
		pluginInnerTabs := buildNestedTabs(pluginProps, "plugin")
		tabsProps["pluginTab"] = Card{
			Type:      "void",
			Component: "FormTab.TabPane",
			ComponentProps: map[string]any{
				"tab": "插件配置",
			},
			Properties: map[string]any{
				"pluginInnerTabs": Card{
					Type:       "void",
					Component:  "FormTab",
					Properties: pluginInnerTabs,
					Index:      0,
				},
			},
			Index: 0,
		}
		
		// 公共配置 Tab - 内部再用嵌套 Tab
		commonInnerTabs := buildNestedTabs(commonProps, "common")
		tabsProps["commonTab"] = Card{
			Type:      "void",
			Component: "FormTab.TabPane",
			ComponentProps: map[string]any{
				"tab": "公共配置",
			},
			Properties: map[string]any{
				"commonInnerTabs": Card{
					Type:       "void",
					Component:  "FormTab",
					Properties: commonInnerTabs,
					Index:      0,
				},
			},
			Index: 1,
		}
		
		fromItems["tabs"] = Card{
			Type:       "void",
			Component:  "FormTab",
			Properties: tabsProps,
			Index:      0,
		}
	} else {
		// 没有分离的情况，使用嵌套 Tab 显示所有配置
		allProps := append(pluginProps, commonProps...)
		innerTabs := buildNestedTabs(allProps, "all")
		fromItems["tabs"] = Card{
			Type:       "void",
			Component:  "FormTab",
			Properties: innerTabs,
			Index:      0,
		}
	}
	return
}
