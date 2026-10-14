package cmd

import "github.com/prismatic-koi/prism/internal/config"

// Theme colour vars used across cmd subcommands.
// Loaded at startup from ~/.config/prism/config.json. Gruvbox-dark defaults
// are used when the file is absent and in a test binary (config.LoadForInit).
var (
	ColorPrimary    string
	ColorSecondary  string
	ColorPurple     string
	ColorYellow     string
	ColorGreen      string
	ColorBlue       string
	ColorRed        string
	ColorForeground string
	ColorBg0        string
)

func init() {
	cfg := config.LoadForInit()
	ColorPrimary = cfg.ColorPrimary
	ColorSecondary = cfg.ColorSecondary
	ColorPurple = cfg.ColorPurple
	ColorYellow = cfg.ColorYellow
	ColorGreen = cfg.ColorGreen
	ColorBlue = cfg.ColorBlue
	ColorRed = cfg.ColorRed
	ColorForeground = cfg.ColorForeground
	ColorBg0 = cfg.ColorBg0
}
