package storefront

import (
	"encoding/json"
	"strings"
)

// OptionGroup is a configurable choice set on a product (size, toppings, etc.).
type OptionGroup struct {
	ID        string        `json:"id"`
	Name      string        `json:"name"`
	Selection string        `json:"selection"` // "single" | "multiple"
	Required  bool          `json:"required"`
	IsActive  bool          `json:"is_active"`
	SortOrder int           `json:"sort_order"`
	Options   []GroupOption `json:"options"`
}

// GroupOption is one choice inside an option group.
type GroupOption struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Price     float64 `json:"price"`
	MaxQty    int     `json:"max_qty"`
	IsActive  bool    `json:"is_active"`
	SortOrder int     `json:"sort_order"`
}

// optionGroupWire is the admin/public JSON shape with pointer bools so missing
// is_active defaults to true.
type optionGroupWire struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Selection string `json:"selection"`
	Required  bool   `json:"required"`
	IsActive  *bool  `json:"is_active"`
	SortOrder int    `json:"sort_order"`
	Options   []struct {
		ID        string  `json:"id"`
		Name      string  `json:"name"`
		Price     float64 `json:"price"`
		MaxQty    int     `json:"max_qty"`
		IsActive  *bool   `json:"is_active"`
		SortOrder int     `json:"sort_order"`
	} `json:"options"`
}

// ParseOptionGroups reads products.addons JSONB as option groups.
// Legacy flat arrays become one optional multi group named "Add-ons".
func ParseOptionGroups(raw []byte) []OptionGroup {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "[]" {
		return []OptionGroup{}
	}
	if looksLikeGroups(raw) {
		return parseGroupsDoc(raw)
	}
	flat := parseFlatAddons(raw)
	if len(flat) == 0 {
		return []OptionGroup{}
	}
	opts := make([]GroupOption, 0, len(flat))
	for i, a := range flat {
		opts = append(opts, GroupOption{
			ID: a.ID, Name: a.Name, Price: a.Price, MaxQty: a.MaxQty,
			IsActive: true, SortOrder: i,
		})
	}
	return []OptionGroup{{
		ID: "addons", Name: "Add-ons", Selection: "multiple",
		Required: false, IsActive: true, SortOrder: 0, Options: opts,
	}}
}

// FlattenOptionGroups returns active options from active groups for pricing.
func FlattenOptionGroups(groups []OptionGroup) []Addon {
	out := []Addon{}
	for _, g := range groups {
		if !g.IsActive {
			continue
		}
		for _, o := range g.Options {
			if !o.IsActive || o.ID == "" {
				continue
			}
			out = append(out, Addon{ID: o.ID, Name: o.Name, Price: o.Price, MaxQty: o.MaxQty})
		}
	}
	return out
}

// EncodeOptionGroups sanitises admin/public groups and returns JSON for storage.
func EncodeOptionGroups(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []byte("[]"), nil
	}
	groups := ParseOptionGroups(raw)
	return encodeGroups(groups)
}

// EncodeOptionGroupList sanitises a typed group slice for storage.
func EncodeOptionGroupList(groups []OptionGroup) ([]byte, error) {
	return encodeGroups(groups)
}

func encodeGroups(groups []OptionGroup) ([]byte, error) {
	cleaned := make([]OptionGroup, 0, len(groups))
	for i, g := range groups {
		name := strings.TrimSpace(g.Name)
		if name == "" {
			continue
		}
		sel := strings.ToLower(strings.TrimSpace(g.Selection))
		if sel != "single" && sel != "multiple" {
			sel = "multiple"
		}
		id := strings.TrimSpace(g.ID)
		if id == "" {
			id = slugify(name, i)
		}
		opts := make([]GroupOption, 0, len(g.Options))
		for j, o := range g.Options {
			oname := strings.TrimSpace(o.Name)
			if oname == "" || o.Price < 0 {
				continue
			}
			oid := strings.TrimSpace(o.ID)
			if oid == "" {
				oid = slugify(oname, j)
			}
			maxQty := o.MaxQty
			if sel == "single" {
				maxQty = 1
			} else if maxQty <= 0 {
				maxQty = 1
			}
			if maxQty > 20 {
				maxQty = 20
			}
			opts = append(opts, GroupOption{
				ID: oid, Name: oname, Price: round2(o.Price), MaxQty: maxQty,
				IsActive: o.IsActive, SortOrder: j,
			})
		}
		if len(opts) == 0 {
			continue
		}
		cleaned = append(cleaned, OptionGroup{
			ID: id, Name: name, Selection: sel, Required: g.Required,
			IsActive: g.IsActive, SortOrder: i, Options: opts,
		})
	}
	if cleaned == nil {
		cleaned = []OptionGroup{}
	}
	return json.Marshal(cleaned)
}

// DecodeOptionGroupsWire unmarshals admin JSON (with optional is_active) into groups.
func DecodeOptionGroupsWire(raw json.RawMessage) ([]OptionGroup, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return []OptionGroup{}, nil
	}
	if !looksLikeGroups(raw) {
		return ParseOptionGroups(raw), nil
	}
	var docs []optionGroupWire
	if err := json.Unmarshal(raw, &docs); err != nil {
		return nil, err
	}
	out := make([]OptionGroup, 0, len(docs))
	for i, d := range docs {
		name := strings.TrimSpace(d.Name)
		if name == "" {
			continue
		}
		sel := strings.ToLower(strings.TrimSpace(d.Selection))
		if sel != "single" && sel != "multiple" {
			sel = "multiple"
		}
		id := strings.TrimSpace(d.ID)
		if id == "" {
			id = slugify(name, i)
		}
		active := true
		if d.IsActive != nil {
			active = *d.IsActive
		}
		opts := make([]GroupOption, 0, len(d.Options))
		for j, o := range d.Options {
			oname := strings.TrimSpace(o.Name)
			if oname == "" || o.Price < 0 {
				continue
			}
			oid := strings.TrimSpace(o.ID)
			if oid == "" {
				oid = slugify(oname, j)
			}
			maxQty := o.MaxQty
			if sel == "single" {
				maxQty = 1
			} else if maxQty <= 0 {
				maxQty = 1
			}
			if maxQty > 20 {
				maxQty = 20
			}
			oActive := true
			if o.IsActive != nil {
				oActive = *o.IsActive
			}
			opts = append(opts, GroupOption{
				ID: oid, Name: oname, Price: round2(o.Price), MaxQty: maxQty,
				IsActive: oActive, SortOrder: j,
			})
		}
		out = append(out, OptionGroup{
			ID: id, Name: name, Selection: sel, Required: d.Required,
			IsActive: active, SortOrder: i, Options: opts,
		})
	}
	return out, nil
}

func looksLikeGroups(raw []byte) bool {
	var probe []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil || len(probe) == 0 {
		return false
	}
	_, hasOptions := probe[0]["options"]
	_, hasSelection := probe[0]["selection"]
	return hasOptions || hasSelection
}

func parseGroupsDoc(raw []byte) []OptionGroup {
	groups, err := DecodeOptionGroupsWire(raw)
	if err != nil {
		return []OptionGroup{}
	}
	return groups
}

func parseFlatAddons(raw []byte) []Addon {
	out := []Addon{}
	var docs []struct {
		ID     string  `json:"id"`
		Name   string  `json:"name"`
		Price  float64 `json:"price"`
		MaxQty int     `json:"max_qty"`
	}
	if err := json.Unmarshal(raw, &docs); err != nil {
		return out
	}
	for i, d := range docs {
		name := strings.TrimSpace(d.Name)
		if name == "" || d.Price < 0 {
			continue
		}
		maxQty := d.MaxQty
		if maxQty <= 0 {
			maxQty = 1
		}
		if maxQty > 20 {
			maxQty = 20
		}
		id := strings.TrimSpace(d.ID)
		if id == "" {
			id = slugify(name, i)
		}
		out = append(out, Addon{ID: id, Name: name, Price: round2(d.Price), MaxQty: maxQty})
	}
	return out
}

// ParseAddons reads add-ons, supporting both group and flat formats.
func ParseAddons(raw []byte) []Addon {
	return FlattenOptionGroups(ParseOptionGroups(raw))
}
