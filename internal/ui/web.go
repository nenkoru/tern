package ui

import "embed"

// Web: the chat-first shell (index.html, shell.js, a2ui.js, style.css): no
// build step, no third-party code, no network beyond its own origin.
//
//go:embed web/*
var Web embed.FS
