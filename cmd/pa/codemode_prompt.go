package main

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/jabing/shutu-agent/internal/llm"
)

// codeModeSDKSection is the host-side equivalent of DSH's generated
// tools:sdk section. The native request still contains only run_code, while
// this section gives the TypeScript program a deterministic typed view of the
// tools available through the nested bridge.
func codeModeSDKSection(specs []llm.ToolSchema) string {
	visible := make([]llm.ToolSchema, 0, len(specs))
	for _, spec := range specs {
		if spec.Name != "run_code" {
			visible = append(visible, spec)
		}
	}
	sort.Slice(visible, func(i, j int) bool { return visible[i].Name < visible[j].Name })

	var b strings.Builder
	b.WriteString("## TypeScript Code Mode SDK\n\n")
	b.WriteString("## Writing code for run_code\n\n")
	b.WriteString("`run_code` takes two required arguments: `code` — the body of an async TypeScript function (erasable syntax only; no `enum` or namespaces; type annotations are advisory, the code runs type-stripped) — and `description`, a short summary of what the program does. Inside the program:\n\n")
	b.WriteString("- Call tools as `await tools.name(args)` — quoted access for exotic names: `tools[\"my-tool\"](args)`. Every call resolves to the tool's typed canonical JSON value. Tool arguments must be lossless JSON.\n")
	b.WriteString("- A FAILED tool call rejects with `ToolCallError`, whose `toolName` identifies the failed tool and whose `message` is human-readable; use `try/catch` to handle and continue.\n")
	b.WriteString("- Independent read-only calls MAY overlap under the host scheduler. Sequence dependent work with `await`.\n")
	b.WriteString("- Emit results with `return` and/or `console.log(...)`. Only what you print or return is program output. Intermediate tool values stay inside the program.\n\n")
	b.WriteString("The available tools:\n\n")
	b.WriteString("```ts\n")
	b.WriteString("type JsonValue = null | boolean | number | string | JsonValue[] | { [key: string]: JsonValue }\n\n")
	b.WriteString("interface ToolArgsMap {\n")
	for _, spec := range visible {
		writeToolDescription(&b, spec.Description, "  ")
		b.WriteString("  ")
		b.WriteString(tsKey(spec.Name))
		b.WriteString(": ")
		b.WriteString(tsSchemaType(spec.Parameters, 1))
		b.WriteString(";\n")
	}
	b.WriteString("}\n\ninterface ToolOutputMap {\n")
	for _, spec := range visible {
		b.WriteString("  ")
		b.WriteString(tsKey(spec.Name))
		b.WriteString(": JsonValue;\n")
	}
	b.WriteString("}\n\n")
	b.WriteString("type ToolName = keyof ToolOutputMap\n\n")
	b.WriteString("declare class ToolCallError extends Error {\n  readonly name: \"ToolCallError\";\n  readonly toolName: ToolName;\n}\n\n")
	b.WriteString("declare const tools: {\n  [K in ToolName]: (args: ToolArgsMap[K]) => Promise<ToolOutputMap[K]>;\n}\n")
	b.WriteString("```\n")
	return b.String()
}

func writeToolDescription(b *strings.Builder, description, indent string) {
	for _, line := range strings.Split(strings.TrimSpace(description), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			b.WriteString(indent)
			b.WriteString("// ")
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
}

func tsKey(name string) string {
	if isTSIdentifier(name) {
		return name
	}
	return strconv.Quote(name)
}

func isTSIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || r == '$' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func tsSchemaType(schema map[string]any, level int) string {
	if schema == nil {
		return "JsonValue"
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		parts := make([]string, 0, len(enum))
		for _, value := range enum {
			encoded, err := json.Marshal(value)
			if err == nil {
				parts = append(parts, string(encoded))
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " | ")
		}
	}
	if unions, ok := schema["anyOf"].([]any); ok && len(unions) > 0 {
		parts := make([]string, 0, len(unions))
		for _, item := range unions {
			if child, ok := item.(map[string]any); ok {
				parts = append(parts, tsSchemaType(child, level))
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, " | ")
		}
	}
	switch schema["type"] {
	case "string":
		return "string"
	case "number", "integer":
		return "number"
	case "boolean":
		return "boolean"
	case "null":
		return "null"
	case "array":
		items, _ := schema["items"].(map[string]any)
		return "(" + tsSchemaType(items, level) + ")[]"
	case "object":
		return tsObjectType(schema, level)
	default:
		return "JsonValue"
	}
}

func tsObjectType(schema map[string]any, level int) string {
	properties, _ := schema["properties"].(map[string]any)
	if len(properties) == 0 {
		if additional, ok := schema["additionalProperties"].(map[string]any); ok {
			return "{ [key: string]: " + tsSchemaType(additional, level+1) + " }"
		}
		return "Record<string, JsonValue>"
	}
	required := map[string]bool{}
	switch values := schema["required"].(type) {
	case []string:
		for _, name := range values {
			required[name] = true
		}
	case []any:
		for _, value := range values {
			if name, ok := value.(string); ok {
				required[name] = true
			}
		}
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	indent := strings.Repeat("  ", level)
	childIndent := strings.Repeat("  ", level+1)
	var b strings.Builder
	b.WriteString("{\n")
	for _, name := range names {
		child, _ := properties[name].(map[string]any)
		b.WriteString(childIndent)
		b.WriteString(tsKey(name))
		if !required[name] {
			b.WriteByte('?')
		}
		b.WriteString(": ")
		b.WriteString(tsSchemaType(child, level+1))
		b.WriteString(";\n")
	}
	b.WriteString(indent)
	b.WriteByte('}')
	return b.String()
}
