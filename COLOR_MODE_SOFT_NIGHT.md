# Soft / Night color_mode support

The frontend Appearance panel now offers Light, Soft, Dark, Night, and System.
Two backend files could not be patched in this environment (owned by another user).
Apply these edits so Soft/Night persist on the account instead of only in the browser.

## `internal/platform/appearance.go`

Allow soft/night in the write validator (~line 270):

```go
if mode != "light" && mode != "soft" && mode != "dark" && mode != "night" && mode != "system" {
    return next, errBadMode
}
```

And in `parsePersonalTheme` (~line 351):

```go
if t.ColorMode != "light" && t.ColorMode != "soft" && t.ColorMode != "dark" && t.ColorMode != "night" && t.ColorMode != "system" {
    t.ColorMode = "system"
}
```

## `internal/platform/catalog.go`

Allow soft/night when setting the business theme (~line 211):

```go
if mode != "light" && mode != "soft" && mode != "dark" && mode != "night" && mode != "system" {
    return "", "", nil, errBadMode
}
```

## Already updated

- `internal/brand/theme.go` — Payload accepts soft/night
- `internal/platform/settings.go` — platform branding accepts soft/night

Until the two files above are patched, the UI still paints Soft/Night live and keeps the choice in `localStorage`, mapping Soft→light and Night→dark for the API
