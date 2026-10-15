package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/akbxr/tego/internal/images"
	"github.com/akbxr/tego/internal/llm"
)

// Image attachments: an image dragged, pasted or typed as a path
// (or pasted with Ctrl+V) becomes a short token such as "🖼 #1" in the
// prompt instead of a long path like /var/folders/…/Screenshot….png. The
// model still gets the image and its path ("[Image #1: /path]").

// attachment is an image waiting to be sent, referenced in the prompt by
// its token.
type attachment struct {
	n     int
	tag   string // "<icon> #n"
	block llm.Block
	info  images.Info
}

// pasteImage is the Ctrl+V hook: it grabs a clipboard image and returns the
// token to insert. On failure it rings the bell and inserts nothing (the
// editor owns the screen while raw).
func (a *app) pasteImage() string {
	blk, info, err := images.FromClipboard()
	if err != nil {
		a.attachMu.Lock()
		a.clipErr = err
		a.attachMu.Unlock()
		fmt.Fprint(os.Stdout, "\a")
		return ""
	}
	return a.newAttachment(blk, info).tag + " "
}

// pasteText is the bracketed-paste hook: image paths in the pasted text
// (dragging a file into the terminal pastes its path) become tokens.
func (a *app) pasteText(text string) string {
	return a.tokenizeImages(text)
}

// newAttachment registers an image and returns its token.
func (a *app) newAttachment(blk llm.Block, info images.Info) attachment {
	a.attachMu.Lock()
	defer a.attachMu.Unlock()
	a.imageSeq++
	at := attachment{n: a.imageSeq, tag: fmt.Sprintf("%s #%d", a.icons.image, a.imageSeq), block: blk, info: info}
	a.pending = append(a.pending, at)
	return at
}

// tokenizeImages replaces readable image paths in text with tokens (a path
// that can't be loaded is left as typed). @mentions are left to
// collectMentions.
func (a *app) tokenizeImages(text string) string {
	matches := images.FindPaths(text, a.cwd)
	for i := len(matches) - 1; i >= 0; i-- { // from the end: offsets stay valid
		m := matches[i]
		if m.Start > 0 && text[m.Start-1] == '@' {
			continue
		}
		blk, info, err := images.Load(m.Path)
		if err != nil {
			continue
		}
		text = text[:m.Start] + a.newAttachment(blk, info).tag + text[m.End:]
	}
	return text
}

// collectImages swaps the tokens in a prompt about to be sent for what the
// model reads ("[Image #1: /path/shot.png]") and returns the image blocks
// in order. Pending images whose token was deleted are dropped.
func (a *app) collectImages(prompt string) (string, []llm.Block) {
	a.attachMu.Lock()
	pending, clipErr := a.pending, a.clipErr
	a.pending, a.clipErr = nil, nil
	a.attachMu.Unlock()
	if clipErr != nil {
		a.ui.Dim("(clipboard: %v)", clipErr)
	}
	var blocks []llm.Block
	for _, at := range pending {
		i := tokenIndex(prompt, at.tag)
		if i < 0 {
			continue
		}
		ref := fmt.Sprintf("[Image #%d]", at.n)
		if src := at.info.Source; src != "" && src != "clipboard" {
			ref = fmt.Sprintf("[Image #%d: %s]", at.n, src) // the model can still refer to the file
		}
		prompt = prompt[:i] + ref + prompt[i+len(at.tag):]
		blocks = append(blocks, at.block)
		a.ui.Dim("📎 %s %s · %d×%d · %s", at.tag, filepath.Base(at.info.Source), at.info.Width, at.info.Height, fmtBytes(at.info.Bytes))
	}
	return prompt, blocks
}

// tokenIndex finds tag in s as a whole token ("#1" must not match "#12").
func tokenIndex(s, tag string) int {
	for off := 0; ; {
		i := strings.Index(s[off:], tag)
		if i < 0 {
			return -1
		}
		i += off
		if end := i + len(tag); end == len(s) || s[end] < '0' || s[end] > '9' {
			return i
		}
		off = i + len(tag)
	}
}

// imageRefRe matches what the model got for an image (and the older
// "path [Image #N]" form), so the transcript can show the token again.
var imageRefRe = regexp.MustCompile(`(?:\S+\.(?i:png|jpe?g|gif|webp) )?\[Image #(\d+)(?:: [^\]\n]*)?\]`)

// imageIcon is the glyph of image tokens in the transcript (set from the
// icon preset at startup).
var imageIcon = iconPresets["unicode"].image

// showImageRefs turns "[Image #1: /path]" back into the token for display.
func showImageRefs(s string) string {
	return imageRefRe.ReplaceAllStringFunc(s, func(m string) string {
		n := imageRefRe.FindStringSubmatch(m)[1]
		if _, err := strconv.Atoi(n); err != nil {
			return m
		}
		return imageIcon + " #" + n
	})
}

func fmtBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}
