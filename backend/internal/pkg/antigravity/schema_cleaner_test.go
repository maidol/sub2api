package antigravity

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanJSONSchema_ArrayPrefixItems(t *testing.T) {
	// 模拟 Claude Code 2.1 Artifact 工具的 query.where 参数 Schema (Draft 2020-12 prefixItems 元组)
	input := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"where": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "array",
							"prefixItems": []any{
								map[string]any{"type": "string"},
								map[string]any{"type": "string", "enum": []any{"==", "!=", ">", "<"}},
								map[string]any{},
							},
						},
						"maxItems": float64(10),
					},
				},
			},
		},
	}

	cleaned := CleanJSONSchema(input)
	require.NotNil(t, cleaned)

	query, ok := cleaned["properties"].(map[string]any)["query"].(map[string]any)
	require.True(t, ok)
	where, ok := query["properties"].(map[string]any)["where"].(map[string]any)
	require.True(t, ok)

	assert.Equal(t, "array", where["type"])

	whereItems, ok := where["items"].(map[string]any)
	require.True(t, ok, "where.items must be an object")
	assert.Equal(t, "array", whereItems["type"])
	// prefixItems 应在 whereItems 中被彻底移除
	assert.Nil(t, whereItems["prefixItems"])

	// 关键验证：where.items.items 必须存在且有效，不能缺失导致 Gemini 400
	innerItems, ok := whereItems["items"].(map[string]any)
	require.True(t, ok, "where.items.items must be an object")
	assert.Equal(t, "string", innerItems["type"])
}

func TestCleanJSONSchema_ArrayMissingItemsFallback(t *testing.T) {
	// 针对任何缺少 items 的 array，必须兜底注入 items: {type: string}
	input := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tags": map[string]any{
				"type": "array",
			},
			"nested_empty_array": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "array",
				},
			},
		},
	}

	cleaned := CleanJSONSchema(input)
	require.NotNil(t, cleaned)

	props, ok := cleaned["properties"].(map[string]any)
	require.True(t, ok)
	tags, ok := props["tags"].(map[string]any)
	require.True(t, ok)
	tagsItems, ok := tags["items"].(map[string]any)
	require.True(t, ok, "tags.items must be an object")
	assert.Equal(t, "string", tagsItems["type"])

	nested, ok := props["nested_empty_array"].(map[string]any)
	require.True(t, ok)
	nestedItems, ok := nested["items"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "array", nestedItems["type"])
	nestedInnerItems, ok := nestedItems["items"].(map[string]any)
	require.True(t, ok, "nested items.items must be an object")
	assert.Equal(t, "string", nestedInnerItems["type"])
}

func TestCleanJSONSchema_ArrayExistingItemsPreserved(t *testing.T) {
	// 正常的 array items 不受影响
	input := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"numbers": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "integer",
				},
			},
		},
	}

	cleaned := CleanJSONSchema(input)
	require.NotNil(t, cleaned)

	props, ok := cleaned["properties"].(map[string]any)
	require.True(t, ok)
	numbers, ok := props["numbers"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "array", numbers["type"])
	items, ok := numbers["items"].(map[string]any)
	require.True(t, ok, "numbers.items must be an object")
	assert.Equal(t, "integer", items["type"])
}

func TestCleanJSONSchema_EnumOnlySchemaInTuple(t *testing.T) {
	// 测试 enum-only schema 不会被误判为 object，也不应被注入 reason 属性
	input := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"choice": map[string]any{
				"enum": []any{"option_a", "option_b"},
			},
			"tuple_with_enum": map[string]any{
				"type": "array",
				"prefixItems": []any{
					map[string]any{
						"enum": []any{"read", "write"},
					},
				},
			},
		},
	}

	cleaned := CleanJSONSchema(input)
	require.NotNil(t, cleaned)

	props, ok := cleaned["properties"].(map[string]any)
	require.True(t, ok)
	choice, ok := props["choice"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "string", choice["type"])
	assert.Nil(t, choice["properties"], "enum-only schema must NOT be treated as object with reason property")
	assert.Equal(t, []any{"option_a", "option_b"}, choice["enum"])

	tupleArray, ok := props["tuple_with_enum"].(map[string]any)
	require.True(t, ok)
	tupleItems, ok := tupleArray["items"].(map[string]any)
	require.True(t, ok, "tuple_with_enum.items must be an object")
	assert.Equal(t, "string", tupleItems["type"])
	assert.Nil(t, tupleItems["properties"])
	assert.Equal(t, []any{"read", "write"}, tupleItems["enum"])
}

func TestCleanJSONSchema_ConstKeywordConversion(t *testing.T) {
	// 测试 const 关键字自动转换为 Gemini 兼容的 enum: [const] 且具有合法 type
	input := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"const": "ping",
			},
		},
	}

	cleaned := CleanJSONSchema(input)
	require.NotNil(t, cleaned)

	props, ok := cleaned["properties"].(map[string]any)
	require.True(t, ok)
	action, ok := props["action"].(map[string]any)
	require.True(t, ok)
	assert.Nil(t, action["const"], "const keyword must be removed")
	assert.Equal(t, "string", action["type"])
	assert.Equal(t, []any{"ping"}, action["enum"])
}

func TestCleanJSONSchema_AnyOfNestedPrefixItems(t *testing.T) {
	// 测试 anyOf 分支合并进来的嵌套 prefixItems 也被完整深度清洗
	input := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"status": map[string]any{"type": "string"},
		},
		"anyOf": []any{
			map[string]any{
				"properties": map[string]any{
					"extra_tuple": map[string]any{
						"type": "array",
						"prefixItems": []any{
							map[string]any{"type": "integer"},
						},
					},
				},
			},
		},
	}

	cleaned := CleanJSONSchema(input)
	require.NotNil(t, cleaned)

	props, ok := cleaned["properties"].(map[string]any)
	require.True(t, ok)
	require.NotNil(t, props["extra_tuple"])
	extraTuple, ok := props["extra_tuple"].(map[string]any)
	require.True(t, ok)
	assert.Nil(t, extraTuple["prefixItems"], "nested prefixItems from anyOf merge must be cleaned")
	extraItems, ok := extraTuple["items"].(map[string]any)
	require.True(t, ok, "extra_tuple.items must be an object")
	assert.Equal(t, "integer", extraItems["type"])
}

func TestCleanJSONSchema_EmptyPrefixItems(t *testing.T) {
	// 验证空 prefixItems: [] 也被安全删除，不遗留非法关键字
	input := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"empty_tuple": map[string]any{
				"type":        "array",
				"prefixItems": []any{},
			},
		},
	}

	cleaned := CleanJSONSchema(input)
	require.NotNil(t, cleaned)

	props, ok := cleaned["properties"].(map[string]any)
	require.True(t, ok)
	emptyTuple, ok := props["empty_tuple"].(map[string]any)
	require.True(t, ok)
	assert.Nil(t, emptyTuple["prefixItems"], "empty prefixItems array must be removed")
	emptyTupleItems, ok := emptyTuple["items"].(map[string]any)
	require.True(t, ok, "empty_tuple.items must be an object")
	assert.Equal(t, "string", emptyTupleItems["type"])
}

// 无参数工具（properties 为空）会被注入占位参数 —— Gemini 不接受空的 function
// declaration。这条锁住「注入用的是保留名，不是 reason」。
func TestCleanJSONSchema_InjectsReservedPlaceholderForEmptyObject(t *testing.T) {
	cleaned := CleanJSONSchema(map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	})

	props, ok := cleaned["properties"].(map[string]any)
	require.True(t, ok, "properties 必须是对象")
	assert.Contains(t, props, PlaceholderArgKey, "空 object 必须注入占位参数")
	// 关键：不能叫 reason。叫 reason 的话回程按名字摘会误伤工具自己的参数。
	assert.NotContains(t, props, "reason", "占位参数名不能是 reason —— 那是常见的合法参数名")
	assert.Equal(t, []any{PlaceholderArgKey}, cleaned["required"])
}

// 反向对照：properties 非空的工具不注入，连带证明上一条不是恒真。
func TestCleanJSONSchema_DoesNotInjectWhenPropsPresent(t *testing.T) {
	cleaned := CleanJSONSchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"reason": map[string]any{"type": "string"},
		},
		"required": []any{"reason"},
	})

	props, ok := cleaned["properties"].(map[string]any)
	require.True(t, ok, "properties 必须是对象")
	assert.NotContains(t, props, PlaceholderArgKey, "已有参数的工具不应被注入占位参数")
	assert.Contains(t, props, "reason", "工具自己的 reason 参数不能被弄丢")
}

func TestStripPlaceholderArgs(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want any
	}{
		{
			name: "摘掉占位参数",
			in:   map[string]any{PlaceholderArgKey: "Check current task list status"},
			want: map[string]any{},
		},
		{
			name: "只摘占位参数，同一次调用里的真实参数留下",
			in: map[string]any{
				PlaceholderArgKey: "why",
				"path":            "/tmp/x",
			},
			want: map[string]any{"path": "/tmp/x"},
		},
		{
			name: "嵌套对象里的占位参数也摘掉",
			in: map[string]any{
				"options": map[string]any{PlaceholderArgKey: "why", "path": "/tmp/x"},
			},
			want: map[string]any{"options": map[string]any{"path": "/tmp/x"}},
		},
		{
			name: "数组元素里的占位参数也摘掉",
			in:   map[string]any{"items": []any{map[string]any{PlaceholderArgKey: "why", "id": 1}}},
			want: map[string]any{"items": []any{map[string]any{"id": 1}}},
		},
		{
			// 这条是占位参数改名的理由：工具自己的 reason 绝不能被摘。
			// 摘掉它是静默破坏 —— 工具照常执行，只是少了一个参数。
			name: "工具自己的 reason 参数原样保留",
			in:   map[string]any{"reason": "watching CI run", "delaySeconds": 600},
			want: map[string]any{"reason": "watching CI run", "delaySeconds": 600},
		},
		{name: "无参数调用", in: map[string]any{}, want: map[string]any{}},
		{name: "nil 原样返回", in: nil, want: nil},
		{name: "非 map 原样返回", in: "not-a-map", want: "not-a-map"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, StripPlaceholderArgs(c.in))
		})
	}
}

// 注入与摘除必须成对：一个无参数工具走完 请求改写 → 模型填参 → 回程还原
// 之后，客户端拿到的入参必须回到空 —— 否则客户端按原始 schema 校验会报
// "An unexpected parameter ... was provided"。
func TestPlaceholderRoundTrip_LeavesNoTrace(t *testing.T) {
	// 请求侧：TaskList 这类无参数工具
	cleaned := CleanJSONSchema(map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	})
	props, ok := cleaned["properties"].(map[string]any)
	require.True(t, ok)
	require.Len(t, props, 1, "请求侧没有注入任何参数，本用例失去意义")

	var injectedKey string
	for k := range props {
		injectedKey = k
	}

	// 模型照着被改写过的 schema 填参
	modelArgs := map[string]any{injectedKey: "Check current task list status"}

	// 回程还原
	restored, err := json.Marshal(StripPlaceholderArgs(modelArgs))
	require.NoError(t, err)
	assert.JSONEq(t, "{}", string(restored), "回程后仍带着网关自己加的参数")
}

// 回程有两条出口，都必须摘：非流式走 NonStreamingProcessor.buildResponse，
// 流式走 StreamingProcessor.processFunctionCall（OpenAI 兼容路径复用后者）。
// 只测 StripPlaceholderArgs 本身证明不了这两处真的调用了它。
func TestPlaceholderStrippedOnBothResponsePaths(t *testing.T) {
	const upstream = `{"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"task_list","args":{"__sub2api_no_args_reason":"why","keep":"me"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2}}}`

	t.Run("非流式", func(t *testing.T) {
		out, _, err := TransformGeminiToClaude([]byte(upstream), "gemini-3.8-flash")
		require.NoError(t, err)

		assert.NotContains(t, string(out), PlaceholderArgKey, "非流式回程没有摘掉占位参数")
		assert.Contains(t, string(out), `"keep"`, "同一次调用里的真实参数被误删了")
	})

	t.Run("流式", func(t *testing.T) {
		p := NewStreamingProcessor("gemini-3.8-flash")
		out := string(p.ProcessLine("data: " + upstream))

		assert.NotContains(t, out, PlaceholderArgKey, "流式回程没有摘掉占位参数")
		assert.Contains(t, out, `keep`, "同一次调用里的真实参数被误删了")
	})
}
