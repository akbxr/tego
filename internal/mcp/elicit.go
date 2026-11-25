package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Elicitation: a server asks the user for input in the middle of a call (to
// confirm a paid or destructive action, to pick an option, or to finish a
// step in the browser). tego shows it, says which server asks, and lets the
// user answer, decline or cancel. Without an Elicitor (tego -p) servers are
// not told tego can, so they don't ask.

// Elicitation is one request for input.
type Elicitation struct {
	Server  string // the server's name in the config
	Mode    string // "form" or "url"
	Message string
	// Form: the fields to fill in, from Schema (requestedSchema as sent).
	Fields []Field
	Schema json.RawMessage
	// URL: the page to open, and the server's id for it ("" without).
	URL string
	ID  string
}

// Field is one input of a form.
type Field struct {
	Name, Title, Description string
	Type                     string // "string", "number", "integer", "boolean"
	Format                   string // a string's: "email", "uri", "date", "date-time"
	Required                 bool
	Options                  []Option // the values a string (or, Multi, each item) may take
	Multi                    bool     // an array of Options' values
	Default                  any
	Min, Max                 *float64 // a number's bounds
	MinLen, MaxLen           *int     // a string's length; Multi: how many items
}

// Option is one value a field may take.
type Option struct{ Value, Title string }

// Label is how the field is named to the user.
func (f Field) Label() string {
	if f.Title != "" {
		return f.Title
	}
	return f.Name
}

// Elicit actions.
const (
	ElicitAccept  = "accept"
	ElicitDecline = "decline"
	ElicitCancel  = "cancel"
)

// ElicitResult is the user's answer: Content only with accept, in a form.
type ElicitResult struct {
	Action  string         `json:"action"`
	Content map[string]any `json:"content,omitempty"`
}

// Elicitor asks the user. It returns cancel when nobody can answer, or
// ctx ends first.
type Elicitor func(ctx context.Context, e *Elicitation) ElicitResult

// elicitParams is elicitation/create's params.
type elicitParams struct {
	Mode            string          `json:"mode"`
	Message         string          `json:"message"`
	RequestedSchema json.RawMessage `json:"requestedSchema"`
	URL             string          `json:"url"`
	ElicitationID   string          `json:"elicitationId"`
}

// parseElicitation reads elicitation/create's params from server.
func parseElicitation(server string, raw json.RawMessage) (*Elicitation, error) {
	var p elicitParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("invalid elicitation: %w", err)
	}
	e := &Elicitation{Server: server, Mode: p.Mode, Message: strings.TrimSpace(p.Message)}
	switch p.Mode {
	case "", "form":
		e.Mode = "form"
		fields, err := ParseSchema(p.RequestedSchema)
		if err != nil {
			return nil, err
		}
		e.Fields, e.Schema = fields, p.RequestedSchema
	case "url":
		u, err := url.Parse(p.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, fmt.Errorf("invalid elicitation url %q", p.URL)
		}
		e.URL, e.ID = p.URL, p.ElicitationID
	default:
		return nil, fmt.Errorf("unknown elicitation mode %q", p.Mode)
	}
	return e, nil
}

// propSchema is one property of a form's schema.
type propSchema struct {
	Type        string          `json:"type"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Format      string          `json:"format"`
	Enum        []string        `json:"enum"`
	EnumNames   []string        `json:"enumNames"` // legacy titles for enum
	OneOf       []constTitle    `json:"oneOf"`
	AnyOf       []constTitle    `json:"anyOf"`
	Items       *propSchema     `json:"items"`
	Default     json.RawMessage `json:"default"`
	Minimum     *float64        `json:"minimum"`
	Maximum     *float64        `json:"maximum"`
	MinLength   *int            `json:"minLength"`
	MaxLength   *int            `json:"maxLength"`
	MinItems    *int            `json:"minItems"`
	MaxItems    *int            `json:"maxItems"`
}

type constTitle struct {
	Const string `json:"const"`
	Title string `json:"title"`
}

func (p *propSchema) options() []Option {
	var out []Option
	for i, v := range p.Enum {
		o := Option{Value: v, Title: v}
		if i < len(p.EnumNames) && p.EnumNames[i] != "" {
			o.Title = p.EnumNames[i]
		}
		out = append(out, o)
	}
	for _, list := range [][]constTitle{p.OneOf, p.AnyOf} {
		for _, c := range list {
			o := Option{Value: c.Const, Title: c.Title}
			if o.Title == "" {
				o.Title = c.Const
			}
			out = append(out, o)
		}
	}
	return out
}

// ParseSchema reads a form's requestedSchema: a flat object of strings,
// numbers, integers, booleans, enums and multi-select enums. The fields come
// in the schema's order.
func ParseSchema(raw json.RawMessage) ([]Field, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s struct {
		Type       string          `json:"type"`
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("invalid form schema: %w", err)
	}
	if s.Type != "" && s.Type != "object" {
		return nil, fmt.Errorf("form schema of type %q, not object", s.Type)
	}
	names, err := objectKeys(s.Properties)
	if err != nil {
		return nil, fmt.Errorf("invalid form schema: %w", err)
	}
	var props map[string]*propSchema
	if len(names) > 0 {
		if err := json.Unmarshal(s.Properties, &props); err != nil {
			return nil, fmt.Errorf("invalid form schema: %w", err)
		}
	}
	required := map[string]bool{}
	for _, r := range s.Required {
		required[r] = true
	}
	fields := make([]Field, 0, len(names))
	for _, name := range names {
		p := props[name]
		if p == nil {
			continue
		}
		f := Field{Name: name, Title: p.Title, Description: p.Description, Type: p.Type, Format: p.Format, Required: required[name],
			Min: p.Minimum, Max: p.Maximum, MinLen: p.MinLength, MaxLen: p.MaxLength}
		switch p.Type {
		case "string", "":
			f.Type = "string"
			f.Options = p.options()
		case "number", "integer", "boolean":
		case "array":
			if p.Items == nil {
				return nil, fmt.Errorf("form field %q: an array without items", name)
			}
			f.Type, f.Multi, f.Options = "string", true, p.Items.options()
			f.MinLen, f.MaxLen = p.MinItems, p.MaxItems
			if len(f.Options) == 0 {
				return nil, fmt.Errorf("form field %q: an array of other than choices", name)
			}
		default:
			return nil, fmt.Errorf("form field %q of type %q", name, p.Type)
		}
		if len(p.Default) > 0 && string(p.Default) != "null" {
			var d any
			if json.Unmarshal(p.Default, &d) == nil {
				f.Default = d
			}
		}
		fields = append(fields, f)
	}
	return fields, nil
}

// objectKeys is raw's keys in order ("" or null: none).
func objectKeys(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("properties is not an object")
	}
	var keys []string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, _ := t.(string)
		keys = append(keys, k)
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// Parse reads what the user typed for f: "" is no value (nil, nil) unless f
// is required. A Multi field takes values separated by commas.
func (f Field) Parse(s string) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		if f.Required {
			return nil, errors.New("required")
		}
		return nil, nil
	}
	if f.Multi {
		var vals []any
		for _, part := range strings.Split(s, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			v, ok := f.option(part)
			if !ok {
				return nil, fmt.Errorf("%q is not one of %s", part, f.optionList())
			}
			vals = append(vals, v)
		}
		if err := f.Check(vals); err != nil {
			return nil, err
		}
		return vals, nil
	}
	var v any
	switch f.Type {
	case "boolean":
		switch strings.ToLower(s) {
		case "y", "yes", "true", "1":
			v = true
		case "n", "no", "false", "0":
			v = false
		default:
			return nil, errors.New("yes or no")
		}
	case "integer":
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, errors.New("a whole number")
		}
		v = n
	case "number":
		n, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, errors.New("a number")
		}
		v = n
	default:
		if len(f.Options) > 0 {
			o, ok := f.option(s)
			if !ok {
				return nil, fmt.Errorf("one of %s", f.optionList())
			}
			v = o
		} else {
			v = s
		}
	}
	if err := f.Check(v); err != nil {
		return nil, err
	}
	return v, nil
}

// option finds the value s names (by value or title, any case).
func (f Field) option(s string) (string, bool) {
	for _, o := range f.Options {
		if strings.EqualFold(o.Value, s) || strings.EqualFold(o.Title, s) {
			return o.Value, true
		}
	}
	return "", false
}

func (f Field) optionList() string {
	var vals []string
	for _, o := range f.Options {
		vals = append(vals, o.Value)
	}
	return strings.Join(vals, ", ")
}

// Check reports what is wrong with v as f's value (nil v: no value).
func (f Field) Check(v any) error {
	if v == nil {
		if f.Required {
			return errors.New("required")
		}
		return nil
	}
	if f.Multi {
		vals, _ := v.([]any)
		if f.MinLen != nil && len(vals) < *f.MinLen {
			return fmt.Errorf("at least %d", *f.MinLen)
		}
		if f.MaxLen != nil && len(vals) > *f.MaxLen {
			return fmt.Errorf("at most %d", *f.MaxLen)
		}
		if f.Required && len(vals) == 0 {
			return errors.New("required")
		}
		return nil
	}
	switch x := v.(type) {
	case int64:
		return f.bounds(float64(x))
	case float64:
		return f.bounds(x)
	case string:
		n := len([]rune(x))
		if f.MinLen != nil && n < *f.MinLen {
			return fmt.Errorf("at least %d characters", *f.MinLen)
		}
		if f.MaxLen != nil && n > *f.MaxLen {
			return fmt.Errorf("at most %d characters", *f.MaxLen)
		}
		return checkFormat(f.Format, x)
	}
	return nil
}

func (f Field) bounds(x float64) error {
	if f.Min != nil && x < *f.Min {
		return fmt.Errorf("at least %v", *f.Min)
	}
	if f.Max != nil && x > *f.Max {
		return fmt.Errorf("at most %v", *f.Max)
	}
	return nil
}

func checkFormat(format, s string) error {
	switch format {
	case "email":
		if a, err := mail.ParseAddress(s); err != nil || a.Address != s {
			return errors.New("an email address")
		}
	case "uri":
		if u, err := url.Parse(s); err != nil || u.Scheme == "" {
			return errors.New("a URI (with a scheme, like https://)")
		}
	case "date":
		if _, err := time.Parse("2006-01-02", s); err != nil {
			return errors.New("a date like 2026-01-31")
		}
	case "date-time":
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			return errors.New("a date and time like 2026-01-31T15:04:05Z")
		}
	}
	return nil
}

// DefaultText is f's default as the user would type it ("" without).
func (f Field) DefaultText() string {
	switch d := f.Default.(type) {
	case nil:
		return ""
	case bool:
		if d {
			return "yes"
		}
		return "no"
	case float64:
		return strconv.FormatFloat(d, 'f', -1, 64)
	case string:
		return d
	case []any:
		var parts []string
		for _, x := range d {
			parts = append(parts, fmt.Sprint(x))
		}
		return strings.Join(parts, ", ")
	}
	return fmt.Sprint(f.Default)
}

// Summary is content as lines "Label: value", in the fields' order.
func Summary(fields []Field, content map[string]any) string {
	var lines []string
	for _, f := range fields {
		v, ok := content[f.Name]
		if !ok {
			continue
		}
		var s string
		switch x := v.(type) {
		case bool:
			s = map[bool]string{true: "yes", false: "no"}[x]
		case []any:
			var parts []string
			for _, p := range x {
				parts = append(parts, fmt.Sprint(p))
			}
			s = strings.Join(parts, ", ")
		default:
			s = fmt.Sprint(x)
		}
		lines = append(lines, f.Label()+": "+s)
	}
	return strings.Join(lines, "\n")
}

// elicit asks the user through s's manager; cancel without an Elicitor.
func (s *server) elicit(ctx context.Context, raw json.RawMessage) (ElicitResult, error) {
	el := s.m.elicit
	if el == nil {
		return ElicitResult{Action: ElicitCancel}, nil
	}
	e, err := parseElicitation(s.cfg.Name, raw)
	if err != nil {
		return ElicitResult{}, err
	}
	r := el(ctx, e)
	switch r.Action {
	case ElicitAccept:
		if e.Mode == "url" {
			r.Content = nil
		}
	case ElicitDecline, ElicitCancel:
		r.Content = nil
	default:
		r = ElicitResult{Action: ElicitCancel}
	}
	return r, nil
}
