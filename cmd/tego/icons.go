package main

// iconSet holds the status-line glyphs. There are two symbol
// presets: "unicode" (works in any font) and "nerd" (Nerd Font glyphs,
// which render larger).
type iconSet struct{ folder, branch, image, video, audio string }

var iconPresets = map[string]iconSet{
	"unicode": {folder: "📁", branch: "⑂", image: "🖼", video: "🎞", audio: "🔊"},
	"nerd":    {folder: "\uf115", branch: "\uf126", image: "\uf03e", video: "\uf03d", audio: "\uf001"}, // nf-fa-folder_open_o, nf-fa-code_fork, nf-fa-image, nf-fa-video_camera, nf-fa-music
}

// icons picks the preset named by the "icons" config value, else unicode.
func icons(configured string) iconSet {
	if p, ok := iconPresets[configured]; ok {
		return p
	}
	return iconPresets["unicode"]
}
