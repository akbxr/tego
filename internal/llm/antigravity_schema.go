package llm

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
)

// cleanSchemaForCCA reduces a tool's JSON Schema to the OpenAPI subset
// Cloud Code Assist accepts in a function declaration's legacy `parameters`
// field: local $refs are inlined, allOf is
// merged, anyOf/oneOf and type arrays are collapsed (null variants dropped),
// const becomes a one-value enum, and every keyword outside
// type/description/properties/required/items/enum is removed. Constraints
// that carry meaning (pattern, minimum, format, …) are appended to the
// description so the model still sees them. The root is always an object.
func cleanSchemaForCCA(raw json.RawMessage) json.RawMessage {
	var root any
	if len(bytes.TrimSpace(raw)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if dec.Decode(&root) != nil {
			root = nil
		}
	}
	c := &ccaCleaner{root: root, refs: map[string]bool{}}
	out := c.node(root, 0)
	if out["type"] != "object" {
		out = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return emptyObjectSchema
	}
	return b
}

type ccaCleaner struct {
	root any
	refs map[string]bool // $refs being expanded (cycle guard)
}

const ccaMaxDepth = 32

// Keywords whose values are moved into the description when stripped.
var ccaSpillKeys = []string{
	"format", "pattern", "minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum",
	"multipleOf", "minLength", "maxLength", "minItems", "maxItems", "uniqueItems",
	"minProperties", "maxProperties", "default", "examples",
}

func (c *ccaCleaner) node(v any, depth int) map[string]any {
	m, ok := v.(map[string]any)
	if !ok || depth > ccaMaxDepth {
		return map[string]any{}
	}
	if ref, ok := m["$ref"].(string); ok {
		target := c.resolve(ref)
		if target == nil || c.refs[ref] {
			m = without(m, "$ref")
		} else {
			c.refs[ref] = true
			merged := map[string]any{}
			for k, val := range target {
				merged[k] = val
			}
			for k, val := range m {
				if k != "$ref" {
					merged[k] = val
				}
			}
			out := c.node(merged, depth+1)
			delete(c.refs, ref)
			return out
		}
	}

	out := map[string]any{}
	desc, _ := m["description"].(string)
	if desc == "" {
		desc, _ = m["title"].(string)
	}
	var spill []string

	// Merge combiners into the node first; explicit keywords win.
	if all, ok := m["allOf"].([]any); ok {
		for _, sub := range all {
			mergeSchema(out, c.node(sub, depth+1))
		}
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		variants, ok := m[key].([]any)
		if !ok {
			continue
		}
		var kept []map[string]any
		for _, sub := range variants {
			s := c.node(sub, depth+1)
			if s["type"] == "null" {
				continue
			}
			kept = append(kept, s)
		}
		if len(kept) == 0 {
			continue
		}
		merged := collapseVariants(kept)
		if types := variantTypes(kept); len(types) > 1 {
			spill = append(spill, "one of: "+strings.Join(types, " | "))
		}
		if d, ok := merged["description"].(string); ok && desc == "" {
			desc = d
		}
		delete(merged, "description")
		mergeSchema(out, merged)
	}

	if t := schemaType(m["type"]); t != "" {
		out["type"] = t
	}
	if props, ok := m["properties"].(map[string]any); ok {
		cleaned, _ := out["properties"].(map[string]any)
		if cleaned == nil {
			cleaned = map[string]any{}
		}
		for name, sub := range props {
			cleaned[name] = c.node(sub, depth+1)
		}
		out["properties"] = cleaned
	}
	if req, ok := m["required"].([]any); ok {
		out["required"] = appendStrings(out["required"], req)
	}
	switch items := m["items"].(type) {
	case map[string]any:
		out["items"] = c.node(items, depth+1)
	case []any: // tuple form: describe by its first element
		if len(items) > 0 {
			out["items"] = c.node(items[0], depth+1)
		}
	}
	if cv, ok := m["const"]; ok {
		if s, ok := cv.(string); ok {
			out["enum"] = []any{s}
		} else {
			spill = append(spill, "const: "+compactJSON(cv))
		}
	}
	if enum, ok := m["enum"].([]any); ok {
		strs := make([]any, 0, len(enum))
		for _, e := range enum {
			if s, ok := e.(string); ok {
				strs = append(strs, s)
			}
		}
		if len(strs) == len(enum) && len(strs) > 0 {
			out["enum"] = strs
		} else {
			spill = append(spill, "enum: "+compactJSON(enum))
		}
	}
	for _, k := range ccaSpillKeys {
		if val, ok := m[k]; ok {
			spill = append(spill, k+": "+compactJSON(val))
		}
	}

	// Infer a missing type; enums are strings on this API.
	if _, ok := out["type"]; !ok {
		switch {
		case out["properties"] != nil:
			out["type"] = "object"
		case out["items"] != nil:
			out["type"] = "array"
		case out["enum"] != nil:
			out["type"] = "string"
		}
	}
	if out["enum"] != nil && out["type"] != "string" {
		spill = append(spill, "enum: "+compactJSON(out["enum"]))
		delete(out, "enum")
	}
	switch out["type"] {
	case "object":
		if out["properties"] == nil {
			out["properties"] = map[string]any{}
		}
		delete(out, "items")
	case "array":
		if out["items"] == nil {
			out["items"] = map[string]any{}
		}
		delete(out, "properties")
		delete(out, "required")
	default:
		delete(out, "properties")
		delete(out, "required")
		delete(out, "items")
	}
	if req, ok := out["required"].([]any); ok {
		props, _ := out["properties"].(map[string]any)
		var kept []any
		seen := map[string]bool{}
		for _, r := range req {
			name, _ := r.(string)
			if _, ok := props[name]; ok && !seen[name] {
				seen[name] = true
				kept = append(kept, name)
			}
		}
		if len(kept) > 0 {
			out["required"] = kept
		} else {
			delete(out, "required")
		}
	}
	if len(spill) > 0 {
		extra := "{" + strings.Join(spill, ", ") + "}"
		if desc != "" {
			desc += "\n" + extra
		} else {
			desc = extra
		}
	}
	if desc != "" {
		out["description"] = desc
	}
	return out
}

// resolve follows a local JSON pointer ("#/$defs/x") into the root schema.
func (c *ccaCleaner) resolve(ref string) map[string]any {
	if ref == "#" {
		m, _ := c.root.(map[string]any)
		return m
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	cur := c.root
	for _, tok := range strings.Split(ref[2:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[tok]
	}
	m, _ := cur.(map[string]any)
	return m
}

// schemaType returns a single type name: the first non-null entry of a type
// array, "" for null-only or missing types.
func schemaType(v any) string {
	switch t := v.(type) {
	case string:
		if t == "null" {
			return ""
		}
		return t
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok && s != "null" {
				return s
			}
		}
	}
	return ""
}

// collapseVariants turns anyOf/oneOf alternatives into one schema: objects
// are merged (no property stays required unless every variant requires
// it), otherwise the first variant wins and same-typed enums are unioned.
func collapseVariants(vs []map[string]any) map[string]any {
	if len(vs) == 1 {
		return vs[0]
	}
	allObjects := true
	for _, v := range vs {
		if v["type"] != "object" {
			allObjects = false
		}
	}
	if allObjects {
		out := map[string]any{"type": "object"}
		props := map[string]any{}
		var common map[string]bool
		for _, v := range vs {
			if p, ok := v["properties"].(map[string]any); ok {
				for k, s := range p {
					if _, dup := props[k]; !dup {
						props[k] = s
					}
				}
			}
			req := map[string]bool{}
			if r, ok := v["required"].([]any); ok {
				for _, x := range r {
					if s, ok := x.(string); ok {
						req[s] = true
					}
				}
			}
			if common == nil {
				common = req
			} else {
				for k := range common {
					if !req[k] {
						delete(common, k)
					}
				}
			}
		}
		out["properties"] = props
		if len(common) > 0 {
			names := make([]string, 0, len(common))
			for k := range common {
				names = append(names, k)
			}
			sort.Strings(names)
			req := make([]any, len(names))
			for i, n := range names {
				req[i] = n
			}
			out["required"] = req
		}
		return out
	}
	out := map[string]any{}
	for k, v := range vs[0] {
		out[k] = v
	}
	if _, ok := out["enum"].([]any); ok {
		seen := map[string]bool{}
		var all []any
		for _, v := range vs {
			if v["type"] != out["type"] {
				continue
			}
			ev, _ := v["enum"].([]any)
			for _, x := range ev {
				if s, ok := x.(string); ok && !seen[s] {
					seen[s] = true
					all = append(all, s)
				}
			}
		}
		out["enum"] = all
	}
	return out
}

func variantTypes(vs []map[string]any) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range vs {
		if t, ok := v["type"].(string); ok && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// mergeSchema folds src into dst: properties and required lists are
// unioned, other keywords are set only when dst lacks them.
func mergeSchema(dst, src map[string]any) {
	for k, v := range src {
		switch k {
		case "properties":
			dp, _ := dst["properties"].(map[string]any)
			if dp == nil {
				dp = map[string]any{}
			}
			if sp, ok := v.(map[string]any); ok {
				for name, s := range sp {
					if _, ok := dp[name]; !ok {
						dp[name] = s
					}
				}
			}
			dst["properties"] = dp
		case "required":
			if r, ok := v.([]any); ok {
				dst["required"] = appendStrings(dst["required"], r)
			}
		case "description":
			if d, ok := dst["description"].(string); !ok || d == "" {
				dst["description"] = v
			}
		default:
			if _, ok := dst[k]; !ok {
				dst[k] = v
			}
		}
	}
}

func appendStrings(existing any, add []any) []any {
	out, _ := existing.([]any)
	for _, x := range add {
		if _, ok := x.(string); ok {
			out = append(out, x)
		}
	}
	return out
}

func without(m map[string]any, key string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	return out
}

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
