package audio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Segment is a stretch of speech, Start..End seconds into what was
// transcribed.
type Segment struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Text  string  `json:"text"`
}

// Transcriber turns a 16 kHz mono WAV file into timed segments.
type Transcriber interface {
	Name() string // e.g. "mlx_whisper (whisper-small-mlx)"
	Transcribe(ctx context.Context, wav string) ([]Segment, error)
}

// Find returns the first local whisper on PATH, or nil: whisper-cli
// (whisper.cpp, with a ggml model found on disk), mlx_whisper, then
// openai-whisper. A variable so tests can stand in a fake.
var Find = find

func find() Transcriber {
	if p, err := exec.LookPath("whisper-cli"); err == nil {
		if m := cppModel(); m != "" {
			return whisperCPP{bin: p, model: m}
		}
	}
	if p, err := exec.LookPath("mlx_whisper"); err == nil {
		return mlxWhisper{bin: p, model: mlxModel()}
	}
	if p, err := exec.LookPath("whisper"); err == nil {
		return openaiWhisper{bin: p, model: ptModel()}
	}
	return nil
}

// InstallHint says how to get a whisper, for the model to pass on.
const InstallHint = "whisper.cpp (brew install whisper-cpp, plus a ggml model such as ggml-base.bin in ~/.cache/whisper.cpp), mlx-whisper (pip install mlx-whisper; Apple Silicon) or openai-whisper (pip install openai-whisper)"

// modelRank orders model names by quality for the time they take: the
// turbo and small models first, the big and tiny ones last. Unknown
// names come after all of them.
func modelRank(name string) int {
	name = strings.ToLower(name)
	for i, k := range []string{"turbo", "small", "base", "medium", "large", "tiny"} {
		if strings.Contains(name, k) {
			r := i * 2
			if strings.Contains(name, ".en") || strings.HasSuffix(name, "-en") {
				r++ // English only: a multilingual one of the same size first
			}
			return r
		}
	}
	return 100
}

// best is the best-ranked of names, or "".
func best(names []string) string {
	if len(names) == 0 {
		return ""
	}
	sort.SliceStable(names, func(i, j int) bool { return modelRank(names[i]) < modelRank(names[j]) })
	return names[0]
}

func home() string {
	h, _ := os.UserHomeDir()
	return h
}

func cacheHome() string {
	if d := os.Getenv("XDG_CACHE_HOME"); d != "" {
		return d
	}
	return filepath.Join(home(), ".cache")
}

// cppModel finds a whisper.cpp ggml model: $WHISPER_CPP_MODEL, else the
// best in the usual places (Homebrew's test models are no use).
func cppModel() string {
	if m := os.Getenv("WHISPER_CPP_MODEL"); m != "" {
		if st, err := os.Stat(m); err == nil && st.Mode().IsRegular() {
			return m
		}
	}
	dirs := []string{
		filepath.Join(cacheHome(), "whisper.cpp"), filepath.Join(cacheHome(), "whisper-cpp"),
		filepath.Join(home(), ".local", "share", "whisper.cpp"), filepath.Join(home(), ".local", "share", "whisper-cpp"),
		filepath.Join(home(), "whisper.cpp", "models"), filepath.Join(home(), ".whisper.cpp"),
		"/opt/homebrew/share/whisper-cpp", "/opt/homebrew/share/whisper.cpp", "/usr/local/share/whisper-cpp",
		"/usr/local/share/whisper.cpp", "/usr/share/whisper-cpp", "/usr/share/whisper.cpp",
	}
	var found []string
	for _, d := range dirs {
		ms, _ := filepath.Glob(filepath.Join(d, "ggml-*.bin"))
		for _, m := range ms {
			if !strings.Contains(filepath.Base(m), "for-tests") {
				found = append(found, m)
			}
		}
	}
	return best(found)
}

// mlxModel is the best mlx-community whisper in the Hugging Face cache, or
// "" for mlx_whisper's default (downloaded on first use).
func mlxModel() string {
	hub := os.Getenv("HF_HUB_CACHE")
	if hub == "" {
		if h := os.Getenv("HF_HOME"); h != "" {
			hub = filepath.Join(h, "hub")
		} else {
			hub = filepath.Join(cacheHome(), "huggingface", "hub")
		}
	}
	dirs, _ := filepath.Glob(filepath.Join(hub, "models--mlx-community--whisper-*"))
	var found []string
	for _, d := range dirs {
		w, _ := filepath.Glob(filepath.Join(d, "snapshots", "*", "weights.*"))
		if len(w) > 0 {
			found = append(found, "mlx-community/"+strings.TrimPrefix(filepath.Base(d), "models--mlx-community--"))
		}
	}
	return best(found)
}

// ptModel is the best openai-whisper model already downloaded, else base
// (downloaded on first use).
func ptModel() string {
	ms, _ := filepath.Glob(filepath.Join(cacheHome(), "whisper", "*.pt"))
	var found []string
	for _, m := range ms {
		found = append(found, strings.TrimSuffix(filepath.Base(m), ".pt"))
	}
	if m := best(found); m != "" {
		return m // the file name is the model's name (large-v3-turbo.pt too)
	}
	return "base"
}

type whisperCPP struct{ bin, model string }

func (w whisperCPP) Name() string {
	return "whisper-cli (" + strings.TrimSuffix(strings.TrimPrefix(filepath.Base(w.model), "ggml-"), ".bin") + ")"
}

func (w whisperCPP) Transcribe(ctx context.Context, wav string) ([]Segment, error) {
	dir, err := os.MkdirTemp("", "tego-whisper-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	base := filepath.Join(dir, "out")
	if err := run(ctx, w.bin, nil, "-m", w.model, "-f", wav, "-l", "auto", "-oj", "-of", base, "-np"); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(base + ".json")
	if err != nil {
		return nil, err
	}
	return parseCPP(data)
}

// parseCPP reads whisper.cpp's -oj output, timed in milliseconds.
func parseCPP(data []byte) ([]Segment, error) {
	var out struct {
		Transcription []struct {
			Offsets struct{ From, To float64 } `json:"offsets"`
			Text    string                     `json:"text"`
		} `json:"transcription"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("whisper-cli output: %w", err)
	}
	segs := make([]Segment, 0, len(out.Transcription))
	for _, t := range out.Transcription {
		segs = append(segs, Segment{Start: t.Offsets.From / 1000, End: t.Offsets.To / 1000, Text: t.Text})
	}
	return segs, nil
}

type mlxWhisper struct{ bin, model string }

func (w mlxWhisper) Name() string {
	if w.model == "" {
		return "mlx_whisper"
	}
	return "mlx_whisper (" + strings.TrimPrefix(w.model, "mlx-community/") + ")"
}

func (w mlxWhisper) Transcribe(ctx context.Context, wav string) ([]Segment, error) {
	dir, err := os.MkdirTemp("", "tego-whisper-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	args := []string{wav, "-f", "json", "-o", dir, "--output-name", "out", "--verbose", "False"}
	var env []string
	if w.model != "" {
		args = append(args, "--model", w.model)
		env = []string{"HF_HUB_OFFLINE=1"} // it is cached: no network check
	}
	if err := run(ctx, w.bin, env, args...); err != nil {
		return nil, err
	}
	return readSegments(filepath.Join(dir, "out.json"))
}

type openaiWhisper struct{ bin, model string }

func (w openaiWhisper) Name() string { return "whisper (" + w.model + ")" }

func (w openaiWhisper) Transcribe(ctx context.Context, wav string) ([]Segment, error) {
	dir, err := os.MkdirTemp("", "tego-whisper-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := run(ctx, w.bin, nil, wav, "--model", w.model, "--output_format", "json", "--output_dir", dir, "--verbose", "False", "--fp16", "False"); err != nil {
		return nil, err
	}
	return readSegments(filepath.Join(dir, strings.TrimSuffix(filepath.Base(wav), filepath.Ext(wav))+".json"))
}

// readSegments reads the JSON that mlx_whisper and openai-whisper write.
func readSegments(path string) ([]Segment, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out struct {
		Segments []Segment `json:"segments"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("whisper output: %w", err)
	}
	return out.Segments, nil
}

func run(ctx context.Context, bin string, env []string, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if msg := lastLine(stderr.String()); msg != "" {
			return errors.New(filepath.Base(bin) + ": " + msg)
		}
		return fmt.Errorf("%s: %w", filepath.Base(bin), err)
	}
	return nil
}

// lastLine is the last non-empty line: a Python traceback ends with the
// error.
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
