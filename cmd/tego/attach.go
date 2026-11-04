package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/akbxr/tego/internal/images"
	"github.com/akbxr/tego/internal/llm"
	"github.com/akbxr/tego/internal/video"
)

// Image attachments: an image dragged, pasted or typed as a path
// (or pasted with Ctrl+V) becomes a short token such as "🖼 #1" in the
// prompt instead of a long path like /var/folders/…/Screenshot….png. The
// model still gets the image and its path ("[Image #1: /path]"). A video
// path becomes "🎞 #2", and the model gets frames of it with their
// timestamps, taken when the prompt is sent.

// attachment is an image (or video) waiting to be sent, referenced in the
// prompt by its token.
type attachment struct {
	n     int
	tag   string // "<icon> #n"
	block llm.Block
	info  images.Info
	video string // a video's path: frames are taken at send time
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

// newVideo registers a video and returns its token.
func (a *app) newVideo(path string) attachment {
	a.attachMu.Lock()
	defer a.attachMu.Unlock()
	a.imageSeq++
	at := attachment{n: a.imageSeq, tag: fmt.Sprintf("%s #%d", a.icons.video, a.imageSeq), video: path}
	a.pending = append(a.pending, at)
	return at
}

// tokenizeImages replaces readable image and video paths in text with
// tokens (a path that can't be loaded is left as typed). @mentions are
// left to collectMentions.
func (a *app) tokenizeImages(text string) string {
	matches := images.FindPathsWith(text, a.cwd, func(s string) bool { return images.HasExt(s) || video.HasExt(s) })
	for i := len(matches) - 1; i >= 0; i-- { // from the end: offsets stay valid
		m := matches[i]
		if m.Start > 0 && text[m.Start-1] == '@' {
			continue
		}
		if video.HasExt(m.Path) {
			text = text[:m.Start] + a.newVideo(m.Path).tag + text[m.End:]
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
		if at.video != "" {
			ref, blks := a.videoAttachment(at)
			prompt = prompt[:i] + ref + prompt[i+len(at.tag):]
			blocks = append(blocks, blks...)
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

// videoAttachment attaches a video: the reference that replaces its token,
// and the blocks after the prompt (what it is, then the video or its
// frames). When it can't be read, the model gets the path and why.
func (a *app) videoAttachment(at attachment) (string, []llm.Block) {
	ref := fmt.Sprintf("[Video #%d: %s]", at.n, at.video)
	desc, blocks, summary, err := a.loadVideo(at.video, at.video)
	if err != nil {
		a.ui.Errorf("%s %s: %v", at.tag, filepath.Base(at.video), err)
		return ref, []llm.Block{{Type: llm.BlockText, Text: fmt.Sprintf("[Video #%d could not be read: %v]", at.n, err)}}
	}
	a.ui.Dim("📎 %s %s · %s", at.tag, filepath.Base(at.video), summary)
	return ref, append([]llm.Block{{Type: llm.BlockText, Text: fmt.Sprintf("[Video #%d] %s", at.n, desc)}}, blocks...)
}

// loadVideo reads the video at path for the current model: the video
// itself, sound included, when it takes video (Gemini), else its frames.
// desc says what the blocks are; summary is for the user.
func (a *app) loadVideo(path, name string) (desc string, blocks []llm.Block, summary string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	note := ""
	if a.ag != nil && llm.TakesVideo(a.ag.Provider, a.ag.Model) {
		clip, blk, err := video.Encode(ctx, path, name, video.Options{})
		if err == nil {
			sound := "with sound"
			if !clip.Audio {
				sound = "no sound"
			}
			return clip.DescribeNative(name) + ". Read it with start/end to watch a part again.", []llm.Block{blk},
				fmt.Sprintf("%s · video, %s · %s", video.Stamp(clip.Duration), sound, fmtBytes(len(blk.Data)/4*3)), nil
		}
		if !errors.Is(err, video.ErrTooLong) && !errors.Is(err, video.ErrTooBig) {
			return "", nil, "", err
		}
		note = fmt.Sprintf(" Frames instead of the video: it is %v; read a part with start/end to watch it with sound.", err)
	}
	clip, err := video.Read(ctx, path, video.Options{})
	if err != nil {
		return "", nil, "", err
	}
	return clip.Describe(name) + ". Read it with start/end/frames for a closer look." + note, clip.Blocks(),
		fmt.Sprintf("%s · %d frames", video.Stamp(clip.Duration), len(clip.Frames)), nil
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

// imageRefRe matches what the model got for an image or video (and the
// older "path [Image #N]" form), so the transcript can show the token
// again.
var imageRefRe = regexp.MustCompile(`(?:\S+\.(?i:png|jpe?g|gif|webp) )?\[(Image|Video) #(\d+)(?:: [^\]\n]*)?\]`)

// imageIcon and videoIcon are the glyphs of attachment tokens in the
// transcript (set from the icon preset at startup).
var (
	imageIcon = iconPresets["unicode"].image
	videoIcon = iconPresets["unicode"].video
)

// showImageRefs turns "[Image #1: /path]" back into the token for display.
func showImageRefs(s string) string {
	return imageRefRe.ReplaceAllStringFunc(s, func(m string) string {
		sub := imageRefRe.FindStringSubmatch(m)
		if _, err := strconv.Atoi(sub[2]); err != nil {
			return m
		}
		if sub[1] == "Video" {
			return videoIcon + " #" + sub[2]
		}
		return imageIcon + " #" + sub[2]
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
