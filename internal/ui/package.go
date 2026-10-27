package ui

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PackageManifest: tern.ui-package/1.
type PackageManifest struct {
	Header
	Kind        string                `json:"kind"`
	Description string                `json:"description,omitempty"`
	A2UI        string                `json:"a2ui"`
	CatalogID   string                `json:"catalogId"`
	Templates   []TemplateRef         `json:"templates"`
	Actions     map[string]ActionDecl `json:"actions"`
	Theme       map[string]string     `json:"theme,omitempty"`
	License     string                `json:"license,omitempty"`
}

type TemplateRef struct {
	ID    string `json:"id"`
	Binds string `json:"binds"`
	File  string `json:"file"`
}

// Package: a checked, loaded UI package.
type Package struct {
	Bundle    *Bundle
	M         PackageManifest
	Templates map[string]Template
}

// themeVars: the only design tokens a package may set (CSS custom properties
// of its own surface box; never the shell's or the trusted layer's).
var themeVars = map[string]bool{"--accent": true, "--bg": true, "--card": true, "--text": true, "--muted": true, "--radius": true,
	"--font": true, "--size": true, "--gap": true, "--border": true, "--title-size": true}

// TemplateNeeds: what each template id must contain to be usable.
var TemplateNeeds = map[string]Requirements{
	"community.feed":  {Actions: []string{"open"}},
	"card.invitation": {},
	"card.job":        {},
	"card.vote":       {},
}

// LoadPackage: the manifest's action kinds are all in the registry, every
// template is a valid surface over the package's own action table, the
// theme sets only design tokens.
func LoadPackage(b *Bundle) (*Package, error) {
	p := &Package{Bundle: b, Templates: map[string]Template{}}
	if err := json.Unmarshal(b.Raw, &p.M); err != nil {
		return nil, err
	}
	if p.M.Schema != KindPackage {
		return nil, fmt.Errorf("%s is a %s, not a UI package", b.Header.Name, p.M.Schema)
	}
	for name, d := range p.M.Actions {
		if !Known(d.Kind) {
			return nil, fmt.Errorf("%s: action %s has kind %q, which is not in the registry", p.M.Name, name, d.Kind)
		}
	}
	for k := range p.M.Theme {
		if !themeVars[k] {
			return nil, fmt.Errorf("%s: theme sets %s; a package sets only %s", p.M.Name, k, strings.Join(keys(themeVars), ", "))
		}
	}
	for _, t := range p.M.Templates {
		raw, err := b.File(t.File)
		if err != nil {
			return nil, err
		}
		tpl, err := ParseScreen(raw)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", p.M.Name, t.File, err)
		}
		s := &Surface{ID: "check", Components: tpl.Components, Data: map[string]any{}, Actions: p.M.Actions}
		if errs := ValidateSurface(s, TemplateNeeds[t.ID]); len(errs) > 0 {
			return nil, fmt.Errorf("%s template %s: %s", p.M.Name, t.ID, strings.Join(errs, "; "))
		}
		p.Templates[t.ID] = tpl
	}
	return p, nil
}

// Surface: a template filled with a data model.
func (p *Package) Surface(template, id string, data map[string]any, tainted bool) (*Surface, error) {
	t, ok := p.Templates[template]
	if !ok {
		return nil, fmt.Errorf("%s has no template %s", p.M.Name, template)
	}
	comps := make([]Component, len(t.Components))
	for i, c := range t.Components {
		cc := Component{}
		for k, v := range c {
			cc[k] = v
		}
		comps[i] = cc
	}
	return &Surface{ID: id, Title: t.Title, Components: comps, Data: data, Actions: p.M.Actions, Tainted: tainted,
		Source: fmt.Sprintf("%s %s by %s", p.M.Name, p.M.Version, p.M.Author), Theme: p.M.Theme}, nil
}
