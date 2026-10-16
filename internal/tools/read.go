package tools

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/akbxr/tego/internal/images"
	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/web"
)

const (
	readDefaultLimit = 2000
	readMaxLineLen   = 2000
	readMaxEntries   = 500
	readMaxBytes     = 50 << 10 // per read: long files are paged
)

type readTool struct{ env *Env }

// ReadOnly: reads only record what the model has seen (goroutine-safe).
func (t *readTool) ReadOnly() bool { return true }

func (t *readTool) Def() llm.Tool {
	return llm.Tool{
		Name:        "read",
		Description: "Read a file (lines prefixed with 1-based line numbers), view an image file (png/jpeg/gif/webp), list a directory, or fetch an http(s) URL as readable text. For large files use offset/limit.",
		Schema: schema(`{"type":"object","properties":{` +
			`"path":{"type":"string","description":"File or directory path, or http(s) URL"},` +
			`"offset":{"type":"integer","description":"1-based start line"},` +
			`"limit":{"type":"integer","description":"Max lines (default 2000)"}},` +
			`"required":["path"]}`),
	}
}

var _ ImageTool = (*readTool)(nil)

// Run is the text-only fallback: image files yield their description only.
func (t *readTool) Run(ctx context.Context, input json.RawMessage) (string, error) {
	text, _, err := t.read(ctx, input)
	return text, err
}

// RunImages returns image files as an image block plus a short description.
func (t *readTool) RunImages(ctx context.Context, input json.RawMessage) (string, []llm.Block, error) {
	return t.read(ctx, input)
}

func (t *readTool) read(ctx context.Context, input json.RawMessage) (string, []llm.Block, error) {
	var in struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decode(input, &in); err != nil {
		return "", nil, err
	}
	if in.Path == "" {
		return "", nil, errors.New("path is required")
	}
	if strings.HasPrefix(in.Path, "http://") || strings.HasPrefix(in.Path, "https://") {
		return readURL(ctx, in.Path, in.Offset, in.Limit)
	}
	p := t.env.abs(in.Path)
	st, err := os.Stat(p)
	if err != nil {
		return "", nil, err
	}
	if st.IsDir() {
		text, err := readDir(p)
		return text, nil, err
	}
	if isImageFile(p) {
		blk, info, err := images.Load(p)
		if err != nil {
			return "", nil, err
		}
		if sum, err := hashFile(p); err == nil {
			t.env.markSeen(p, sum)
		}
		text := fmt.Sprintf("Image %s (%dx%d, %d KB)", t.env.rel(p), info.Width, info.Height, (info.Bytes+1023)/1024)
		return text, []llm.Block{blk}, nil
	}
	var sum [sha256.Size]byte
	text, err := t.env.readFile(ctx, p, in.Offset, in.Limit, &sum)
	if err == nil {
		t.env.markSeen(p, sum) // any range counts: edit matches text, not line numbers
	}
	return text, nil, err
}

// readURL fetches a web page; its text is windowed like a file, without
// line numbers, below a URL/title header.
func readURL(ctx context.Context, rawURL string, offset, limit int) (string, []llm.Block, error) {
	page, err := web.Fetch(ctx, rawURL)
	if err != nil {
		return "", nil, err
	}
	if page.Image != nil {
		return page.Text, []llm.Block{*page.Image}, nil
	}
	head := page.URL + "\n" + page.Title + "\n\n"
	if page.Text == "" {
		return head + "(no readable text)", nil, nil
	}
	lines := strings.Split(page.Text, "\n")
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = readDefaultLimit
	}
	if offset > len(lines) {
		return "", nil, fmt.Errorf("offset %d is past end of page (%d lines)", offset, len(lines))
	}
	last := min(offset+limit-1, len(lines))
	var b strings.Builder
	b.WriteString(head)
	for i, s := range lines[offset-1 : last] {
		if len(s) > readMaxLineLen {
			s = strings.ToValidUTF8(s[:readMaxLineLen], "") + " …[line truncated]"
		}
		if i > 0 && b.Len()+len(s) > readMaxBytes {
			last = offset + i - 1
			break
		}
		b.WriteString(s)
		b.WriteByte('\n')
	}
	if last < len(lines) {
		fmt.Fprintf(&b, "[showing lines %d-%d of %d; use offset=%d to continue]\n", offset, last, len(lines), last+1)
	}
	if page.Truncated {
		b.WriteString("[page truncated at 5 MB]\n")
	}
	return strings.TrimSuffix(b.String(), "\n"), nil, nil
}

// isImageFile reports whether p starts with PNG, JPEG, GIF or WebP magic bytes.
func isImageFile(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var head [12]byte
	n, _ := io.ReadFull(f, head[:])
	return images.MediaType(head[:n]) != ""
}

func readDir(p string) (string, error) {
	ents, err := os.ReadDir(p)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() {
			n += "/"
		} else if e.Type()&os.ModeSymlink != 0 {
			if st, err := os.Stat(p + "/" + n); err == nil && st.IsDir() {
				n += "/"
			}
		}
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "(empty directory)", nil
	}
	var b strings.Builder
	for i, n := range names {
		if i == readMaxEntries {
			fmt.Fprintf(&b, "... %d more entries\n", len(names)-readMaxEntries)
			break
		}
		b.WriteString(n)
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// readFile renders lines [offset, offset+limit) of p with line numbers. sum
// receives the hash of the whole file (it is read to the end anyway to
// count lines); it is left zero for binary files.
func readFile(p string, offset, limit int, sum *[sha256.Size]byte) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return numberLines(p, f, func() int64 {
		st, _ := f.Stat()
		return st.Size()
	}, offset, limit, sum)
}

// numberLines renders lines offset..offset+limit-1 of src (the file p)
// with their numbers, and hashes all of src into sum.
func numberLines(p string, src io.Reader, size func() int64, offset, limit int, sum *[sha256.Size]byte) (string, error) {
	h := sha256.New()
	r := bufio.NewReaderSize(io.TeeReader(src, h), 64<<10)
	if head, _ := r.Peek(8192); isBinary(head) {
		return "", fmt.Errorf("%s is a binary file (%d bytes); not displayed", p, size())
	}
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = readDefaultLimit
	}
	end := offset + limit - 1 // last line to show

	var b strings.Builder
	line, shown := 0, 0
	full := false // the byte budget is spent; keep counting lines (and hashing)
	for {
		s, err := readLineBounded(r, readMaxLineLen+1)
		if len(s) > 0 || err == nil {
			line++
			if line >= offset && line <= end && !full {
				s = strings.TrimRight(s, "\r\n")
				if len(s) > readMaxLineLen {
					s = strings.ToValidUTF8(s[:readMaxLineLen], "") + " …[line truncated]"
				}
				row := strconv.Itoa(line) + "\t" + s + "\n"
				if shown > 0 && b.Len()+len(row) > readMaxBytes {
					full = true
				} else {
					b.WriteString(row)
					shown++
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	copy(sum[:], h.Sum(nil)) // the whole file went through h
	if line == 0 {
		return "(empty file)", nil
	}
	if offset > line {
		return "", fmt.Errorf("offset %d is past end of file (%d lines)", offset, line)
	}
	if last := offset + shown - 1; last < line {
		limit := ""
		if full {
			limit = fmt.Sprintf(" (%d KB per read)", readMaxBytes>>10)
		}
		fmt.Fprintf(&b, "[showing lines %d-%d of %d%s; use offset=%d to continue]", offset, last, line, limit, last+1)
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// readLineBounded reads one line (including its '\n') from r, keeping at most
// max bytes; the remainder of an overlong line is discarded. err is io.EOF at
// end of input (s may hold a final unterminated line).
func readLineBounded(r *bufio.Reader, max int) (string, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if room := max - len(buf); room > 0 {
			if len(chunk) > room {
				buf = append(buf, chunk[:room]...)
			} else {
				buf = append(buf, chunk...)
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return string(buf), err
	}
}
