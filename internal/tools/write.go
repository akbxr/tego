package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/akbxr/tego/internal/llm"
)

type writeTool struct{ env *Env }

func (t *writeTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "write",
		Description: "Create or overwrite a file with the given content (parent dirs are created). Overwriting an existing file requires reading it with the read tool in this session; otherwise nothing is written and the error returns the current content, then retry. Prefer edit for changing existing files.",
		Schema: schema(`{"type":"object","properties":{` +
			`"path":{"type":"string"},` +
			`"content":{"type":"string"}},` +
			`"required":["path","content"]}`),
	}
}

func (t *writeTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var in struct {
		Path    string  `json:"path"`
		Content *string `json:"content"`
	}
	if err := decode(input, &in); err != nil {
		return "", err
	}
	if in.Path == "" {
		return "", errors.New("path is required")
	}
	if in.Content == nil {
		return "", errors.New("content is required")
	}
	p := t.env.abs(in.Path)
	mode := os.FileMode(0o644)
	verb := "Created"
	if st, err := os.Stat(p); err == nil {
		if st.IsDir() {
			return "", fmt.Errorf("%s is a directory", in.Path)
		}
		sum, err := t.env.hash(ctx, p)
		if err != nil {
			return "", err
		}
		if err := t.env.checkSeen(p, sum, "overwrite"); err != nil {
			return "", err
		}
		mode = st.Mode().Perm()
		verb = "Overwrote"
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	if err := t.env.writeText(ctx, p, []byte(*in.Content), mode); err != nil {
		return "", err
	}
	t.env.markSeen(p, sha256.Sum256([]byte(*in.Content)))
	return fmt.Sprintf("%s %s (%d bytes, %d lines)", verb, t.env.rel(p), len(*in.Content), countLines(*in.Content)) + t.env.diagnose(ctx, p), nil
}
