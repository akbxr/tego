package config

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/akbxr/tego/internal/llm"
)

// SubagentModel is a model subagents can run on besides the session's and
// the small one, from another subscription or provider if need be. In
// config.json it is "name": "provider/model" or "name": {"model": …,
// "use": …, "thinking": …}.
type SubagentModel struct {
	Model    string `json:"model"`              // provider/model (a bare model id: one of the session's provider)
	Use      string `json:"use,omitempty"`      // what it is good for, told to the session's model
	Thinking string `json:"thinking,omitempty"` // its thinking level (default: the session's)
}

func (m *SubagentModel) UnmarshalJSON(data []byte) error {
	var spec string
	if json.Unmarshal(data, &spec) == nil {
		*m = SubagentModel{Model: spec}
		return nil
	}
	type plain SubagentModel
	return json.Unmarshal(data, (*plain)(m))
}

func (m SubagentModel) MarshalJSON() ([]byte, error) {
	if m.Use == "" && m.Thinking == "" {
		return json.Marshal(m.Model) // kept as the user wrote it
	}
	type plain SubagentModel
	return json.Marshal(plain(m))
}

// NamedSubagentModel is an entry of subagent_models.
type NamedSubagentModel struct {
	Name string
	SubagentModel
}

var subagentModelName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// validateSubagentModels checks subagent_models when the config loads.
func (c *Config) validateSubagentModels() error {
	for _, m := range c.SubagentModelList() {
		switch {
		case m.Name == "main" || m.Name == "small":
			return fmt.Errorf("subagent_models: %q is taken by a built-in choice; pick another name", m.Name)
		case !subagentModelName.MatchString(m.Name):
			return fmt.Errorf("subagent_models: name %q: use lowercase letters, digits, '.', '_' or '-'", m.Name)
		case strings.TrimSpace(m.Model) == "":
			return fmt.Errorf("subagent_models.%s: model is empty (provider/model)", m.Name)
		case m.Thinking != "" && llm.ThinkingRank(m.Thinking) < 0:
			return fmt.Errorf("subagent_models.%s: invalid thinking level %q", m.Name, m.Thinking)
		}
	}
	return nil
}

// SubagentModelList is subagent_models sorted by name.
func (c *Config) SubagentModelList() []NamedSubagentModel {
	out := make([]NamedSubagentModel, 0, len(c.SubModels))
	for name, m := range c.SubModels {
		out = append(out, NamedSubagentModel{Name: name, SubagentModel: m})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SubagentModelSpec is the provider/model spec of m in a session on
// provider: a bare model id is one of that provider's.
func SubagentModelSpec(m SubagentModel, provider string) string {
	if strings.Contains(m.Model, "/") || provider == "" {
		return m.Model
	}
	return provider + "/" + m.Model
}
