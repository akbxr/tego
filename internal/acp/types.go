package acp

import "encoding/json"

// Types of ACP v1 (schema/v1/schema.json), as far as tego uses them.

const ProtocolVersion = 1

type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type InitializeRequest struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities ClientCapabilities `json:"clientCapabilities"`
	ClientInfo         *Implementation    `json:"clientInfo,omitempty"`
}

type ClientCapabilities struct {
	FS       FileSystemCapabilities `json:"fs"`
	Terminal bool                   `json:"terminal"`
}

type FileSystemCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type InitializeResponse struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentCapabilities AgentCapabilities `json:"agentCapabilities"`
	AuthMethods       []AuthMethod      `json:"authMethods"`
	AgentInfo         *Implementation   `json:"agentInfo,omitempty"`
}

// AuthMethod is one way to log in; tego's are run in a terminal.
type AuthMethod struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Type        string   `json:"type,omitempty"` // "terminal"
	Args        []string `json:"args,omitempty"`
}

type AgentCapabilities struct {
	LoadSession         bool                `json:"loadSession"`
	PromptCapabilities  PromptCapabilities  `json:"promptCapabilities"`
	MCPCapabilities     MCPCapabilities     `json:"mcpCapabilities"`
	SessionCapabilities SessionCapabilities `json:"sessionCapabilities"`
}

type PromptCapabilities struct {
	Image           bool `json:"image"`
	Audio           bool `json:"audio"`
	EmbeddedContext bool `json:"embeddedContext"`
}

type MCPCapabilities struct {
	HTTP bool `json:"http"`
	SSE  bool `json:"sse"`
}

// SessionCapabilities: an empty object advertises a method, nil not.
type SessionCapabilities struct {
	List   *struct{} `json:"list,omitempty"`
	Resume *struct{} `json:"resume,omitempty"`
	Close  *struct{} `json:"close,omitempty"`
}

// MCPServer is any of the three transports (type "http", "sse", or none
// for stdio).
type MCPServer struct {
	Type    string        `json:"type,omitempty"`
	Name    string        `json:"name"`
	Command string        `json:"command,omitempty"`
	Args    []string      `json:"args,omitempty"`
	Env     []EnvVariable `json:"env,omitempty"`
	URL     string        `json:"url,omitempty"`
	Headers []EnvVariable `json:"headers,omitempty"` // name/value, like env
}

type EnvVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type NewSessionRequest struct {
	Cwd        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers"`
}

// SessionSetup is the response to session/new, session/load and
// session/resume (only session/new has the id).
type SessionSetup struct {
	SessionID     string         `json:"sessionId,omitempty"`
	Modes         *ModeState     `json:"modes,omitempty"`
	ConfigOptions []ConfigOption `json:"configOptions,omitempty"`
}

type LoadSessionRequest struct {
	SessionID  string      `json:"sessionId"`
	Cwd        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers"`
}

type ListSessionsRequest struct {
	Cwd    string `json:"cwd,omitempty"`
	Cursor string `json:"cursor,omitempty"`
}

type ListSessionsResponse struct {
	Sessions   []SessionInfo `json:"sessions"`
	NextCursor string        `json:"nextCursor,omitempty"`
}

type SessionInfo struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Title     string `json:"title,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty"`
}

type SessionRef struct {
	SessionID string `json:"sessionId"`
}

type ModeState struct {
	CurrentModeID  string `json:"currentModeId"`
	AvailableModes []Mode `json:"availableModes"`
}

type Mode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type SetModeRequest struct {
	SessionID string `json:"sessionId"`
	ModeID    string `json:"modeId"`
}

// ConfigOption is a select option (the only type tego uses).
type ConfigOption struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Description  string         `json:"description,omitempty"`
	Category     string         `json:"category,omitempty"` // model, thought_level, mode
	Type         string         `json:"type"`               // "select"
	CurrentValue string         `json:"currentValue"`
	Options      []SelectOption `json:"options"`
}

type SelectOption struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type SetConfigOptionRequest struct {
	SessionID string          `json:"sessionId"`
	ConfigID  string          `json:"configId"`
	Value     json.RawMessage `json:"value"`
}

type SetConfigOptionResponse struct {
	ConfigOptions []ConfigOption `json:"configOptions"`
}

type PromptRequest struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

type PromptResponse struct {
	StopReason string `json:"stopReason"` // end_turn, max_tokens, max_turn_requests, refusal, cancelled
}

// ContentBlock is text, an image, a resource link or an embedded resource.
type ContentBlock struct {
	Type     string            `json:"type"`
	Text     string            `json:"text,omitempty"`
	Data     string            `json:"data,omitempty"` // image: base64
	MimeType string            `json:"mimeType,omitempty"`
	URI      string            `json:"uri,omitempty"` // resource_link
	Name     string            `json:"name,omitempty"`
	Resource *EmbeddedResource `json:"resource,omitempty"`
}

type EmbeddedResource struct {
	URI      string `json:"uri"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// Text is a text content block.
func Text(s string) ContentBlock { return ContentBlock{Type: "text", Text: s} }

type SessionNotification struct {
	SessionID string `json:"sessionId"`
	Update    any    `json:"update"`
}

// Chunk is a user_message_chunk, agent_message_chunk or agent_thought_chunk.
type Chunk struct {
	SessionUpdate string       `json:"sessionUpdate"`
	Content       ContentBlock `json:"content"`
}

// ToolCall is a tool_call update, or a tool_call_update when SessionUpdate
// says so (then every field but the id is optional).
type ToolCall struct {
	SessionUpdate string            `json:"sessionUpdate,omitempty"`
	ToolCallID    string            `json:"toolCallId"`
	Title         string            `json:"title,omitempty"`
	Kind          string            `json:"kind,omitempty"`   // read, edit, delete, move, search, execute, think, fetch, other
	Status        string            `json:"status,omitempty"` // pending, in_progress, completed, failed
	Content       []ToolCallContent `json:"content,omitempty"`
	Locations     []Location        `json:"locations,omitempty"`
	RawInput      json.RawMessage   `json:"rawInput,omitempty"`
}

// ToolCallContent is content, a diff or a terminal.
type ToolCallContent struct {
	Type       string        `json:"type"`
	Content    *ContentBlock `json:"content,omitempty"`
	Path       string        `json:"path,omitempty"`
	OldText    *string       `json:"oldText,omitempty"`
	NewText    *string       `json:"newText,omitempty"`
	TerminalID string        `json:"terminalId,omitempty"`
}

type Location struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
}

type PlanUpdate struct {
	SessionUpdate string      `json:"sessionUpdate"` // "plan"
	Entries       []PlanEntry `json:"entries"`
}

type PlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority"` // high, medium, low
	Status   string `json:"status"`   // pending, in_progress, completed
}

type CommandsUpdate struct {
	SessionUpdate     string    `json:"sessionUpdate"` // "available_commands_update"
	AvailableCommands []Command `json:"availableCommands"`
}

type Command struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	Input       *CommandInput `json:"input,omitempty"`
}

type CommandInput struct {
	Hint string `json:"hint"`
}

type ModeUpdate struct {
	SessionUpdate string `json:"sessionUpdate"` // "current_mode_update"
	CurrentModeID string `json:"currentModeId"`
}

type ConfigOptionUpdate struct {
	SessionUpdate string         `json:"sessionUpdate"` // "config_option_update"
	ConfigOptions []ConfigOption `json:"configOptions"`
}

type SessionInfoUpdate struct {
	SessionUpdate string `json:"sessionUpdate"` // "session_info_update"
	Title         string `json:"title,omitempty"`
	UpdatedAt     string `json:"updatedAt,omitempty"`
}

type PermissionRequest struct {
	SessionID string             `json:"sessionId"`
	ToolCall  ToolCall           `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // allow_once, allow_always, reject_once, reject_always
}

type PermissionResponse struct {
	Outcome struct {
		Outcome  string `json:"outcome"` // cancelled, selected
		OptionID string `json:"optionId,omitempty"`
	} `json:"outcome"`
}

type ReadTextFileRequest struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
}

type ReadTextFileResponse struct {
	Content string `json:"content"`
}

type WriteTextFileRequest struct {
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Content   string `json:"content"`
}

type CreateTerminalRequest struct {
	SessionID       string        `json:"sessionId"`
	Command         string        `json:"command"`
	Args            []string      `json:"args,omitempty"`
	Env             []EnvVariable `json:"env,omitempty"`
	Cwd             string        `json:"cwd,omitempty"`
	OutputByteLimit int           `json:"outputByteLimit,omitempty"`
}

type CreateTerminalResponse struct {
	TerminalID string `json:"terminalId"`
}

type TerminalRef struct {
	SessionID  string `json:"sessionId"`
	TerminalID string `json:"terminalId"`
}

type TerminalOutputResponse struct {
	Output     string      `json:"output"`
	Truncated  bool        `json:"truncated"`
	ExitStatus *ExitStatus `json:"exitStatus,omitempty"`
}

type ExitStatus struct {
	ExitCode *int   `json:"exitCode,omitempty"`
	Signal   string `json:"signal,omitempty"`
}
