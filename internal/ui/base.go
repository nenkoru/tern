package ui

import (
	"embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

//go:embed base/manifest.json base/templates/*.json
var baseFS embed.FS

var (
	baseOnce sync.Once
	basePkg  *Package
	baseErr  error
)

// Base: the device's own templates (part of the client, like the shell):
// what renders a feed or a card when no installed package has a template for it.
func Base() (*Package, error) {
	baseOnce.Do(func() {
		raw, _ := baseFS.ReadFile("base/manifest.json")
		p := &Package{Templates: map[string]Template{}}
		if baseErr = json.Unmarshal(raw, &p.M); baseErr != nil {
			return
		}
		for _, t := range p.M.Templates {
			b, err := baseFS.ReadFile("base/" + t.File)
			if err != nil {
				baseErr = err
				return
			}
			tpl, err := ParseScreen(b)
			if err != nil {
				baseErr = fmt.Errorf("base %s: %w", t.File, err)
				return
			}
			s := &Surface{ID: "check", Components: tpl.Components, Data: map[string]any{}, Actions: p.M.Actions}
			if errs := ValidateSurface(s, TemplateNeeds[t.ID]); len(errs) > 0 {
				baseErr = fmt.Errorf("base template %s: %s", t.ID, strings.Join(errs, "; "))
				return
			}
			p.Templates[t.ID] = tpl
		}
		basePkg = p
	})
	return basePkg, baseErr
}

// BaseActions: the action table generated screens may use (the agent's own,
// over the feed's data model): generated screens can't invent action kinds.
func BaseActions() map[string]ActionDecl {
	b, err := Base()
	if err != nil {
		return nil
	}
	out := map[string]ActionDecl{}
	for _, k := range []string{"up", "down", "open", "tip"} {
		out[k] = b.M.Actions[k]
	}
	return out
}
