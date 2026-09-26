package brand

import (
	"encoding/json"
	"strings"
)

// Tokens are the CSS values stored on a theme preset.
type Tokens struct {
	Accent      string `json:"accent"`
	Accent2     string `json:"accent2"`
	RadiusSm    string `json:"radius_sm"`
	Radius      string `json:"radius"`
	RadiusLg    string `json:"radius_lg"`
	FontDisplay string `json:"font_display"`
	FontBody    string `json:"font_body"`
}

func ParseTokens(raw []byte) Tokens {
	var t Tokens
	_ = json.Unmarshal(raw, &t)
	if t.Accent == "" {
		t.Accent = "#4f46e5"
	}
	if t.Accent2 == "" {
		t.Accent2 = t.Accent
	}
	if t.RadiusSm == "" {
		t.RadiusSm = "6px"
	}
	if t.Radius == "" {
		t.Radius = "8px"
	}
	if t.RadiusLg == "" {
		t.RadiusLg = "12px"
	}
	if t.FontDisplay == "" {
		t.FontDisplay = "Inter"
	}
	if t.FontBody == "" {
		t.FontBody = "Inter"
	}
	return t
}

// Merge applies tenant overrides on top of a preset. Only accent colors are overridable.
func Merge(presetJSON, overridesJSON []byte) Tokens {
	base := ParseTokens(presetJSON)
	if len(overridesJSON) == 0 {
		return base
	}
	var over struct {
		Accent  string `json:"accent"`
		Accent2 string `json:"accent2"`
	}
	if err := json.Unmarshal(overridesJSON, &over); err != nil {
		return base
	}
	if strings.TrimSpace(over.Accent) != "" {
		base.Accent = over.Accent
	}
	if strings.TrimSpace(over.Accent2) != "" {
		base.Accent2 = over.Accent2
	}
	return base
}

// Payload is the theme object returned to clients.
func Payload(presetID, presetName, mode string, presetJSON, overridesJSON []byte) map[string]any {
	if mode != "light" && mode != "dark" && mode != "system" {
		mode = "system"
	}
	name := presetName
	if name == "" {
		name = presetID
	}
	return map[string]any{
		"preset_id":   presetID,
		"preset_name": name,
		"color_mode":  mode,
		"tokens":      Merge(presetJSON, overridesJSON),
	}
}
