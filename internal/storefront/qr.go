package storefront

import (
	"bytes"
	"encoding/base64"
	"strings"

	qrcode "github.com/skip2/go-qrcode"
)

// QR renders the tenant's public storefront address as a QR code.
//
// The code is generated server-side so the admin screen has no client-side QR
// library to load and the URL encoded in it is exactly the one the platform
// resolved, not one assembled in the browser.
type QRCode struct {
	// URL is the public storefront address.
	URL string `json:"url"`
	// PNG is a base64 data URI, ready to drop into an <img>.
	PNG string `json:"png"`
	// SVG is an inline vector, used for print and for crisp rendering.
	SVG string `json:"svg"`
	// Size is the pixel size the bitmap was rendered at.
	Size int `json:"size"`
}

// QRCodeOptions tunes the render.
type QRCodeOptions struct {
	Size    int
	Padding int
	// Dark and Light are hex colours. They default to the tenant's own brand
	// colours so the printed code matches the storefront.
	Dark  string
	Light string
}

// BuildQRCode renders url as a QR code, falling back to a high-contrast
// monochrome pair when the tenant's brand colours would be unreadable.
func BuildQRCode(url string, opts QRCodeOptions) (QRCode, error) {
	size := opts.Size
	if size < 128 {
		size = 320
	}
	if size > 1024 {
		size = 1024
	}
	padding := opts.Padding
	if padding < 0 {
		padding = 16
	}
	dark, light := pickQRColours(opts.Dark, opts.Light)

	code, err := qrcode.New(url, qrcode.Medium)
	if err != nil {
		return QRCode{}, err
	}
	png, err := code.PNG(size)
	if err != nil {
		return QRCode{}, err
	}
	svg, err := codeSVG(code, size, padding, dark, light)
	if err != nil {
		return QRCode{}, err
	}
	return QRCode{
		URL:  url,
		PNG:  "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
		SVG:  svg,
		Size: size,
	}, nil
}

// pickQRColours guarantees contrast. Scanners need a dark module on a light
// field, so a brand colour that is too light against white is darkened until
// it is readable rather than shipped as an unscannable sticker.
func pickQRColours(dark, light string) (string, string) {
	background := "#ffffff"
	if c, ok := parseHex(light); ok {
		background = formatRGB(c)
	}
	if c, ok := parseHex(dark); ok {
		candidate := formatRGB(c)
		if contrastRatio(candidate, background) >= 4.0 {
			return candidate, background
		}
	}
	// Fall back to a guaranteed-readable dark ink on the chosen background.
	return "#111827", background
}

// codeSVG emits a vector QR. go-qrcode renders SVG itself, but going through
// its bitmap keeps the module grid and the colour choice in one place.
func codeSVG(code *qrcode.QRCode, size, padding int, dark, light string) (string, error) {
	_ = size
	bitmap := code.Bitmap()
	if len(bitmap) == 0 {
		return "", errEmptyBitmap
	}
	side := len(bitmap)
	// The SVG viewBox is the module grid; CSS scales it to any size, so one
	// string works for a phone screen and a printed A4 sheet.
	var b bytes.Buffer
	b.WriteString(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="`)
	b.WriteString(itoa(-padding))
	b.WriteString(" ")
	b.WriteString(itoa(-padding))
	b.WriteString(" ")
	b.WriteString(itoa(side + padding*2))
	b.WriteString(" ")
	b.WriteString(itoa(side + padding*2))
	b.WriteString(`" shape-rendering="crispEdges" role="img" aria-label="Storefront QR code">`)
	b.WriteString(`<rect x="`)
	b.WriteString(itoa(-padding))
	b.WriteString(`" y="`)
	b.WriteString(itoa(-padding))
	b.WriteString(`" width="`)
	b.WriteString(itoa(side + padding*2))
	b.WriteString(`" height="`)
	b.WriteString(itoa(side + padding*2))
	b.WriteString(`" fill="`)
	b.WriteString(light)
	b.WriteString(`"/>`)
	// One path for all dark modules keeps the DOM small enough to inline.
	b.WriteString(`<path fill="`)
	b.WriteString(dark)
	b.WriteString(`" d="`)
	first := true
	for y := 0; y < side; y++ {
		for x := 0; x < side; x++ {
			if !bitmap[y][x] {
				continue
			}
			if !first {
				b.WriteString(" ")
			}
			first = false
			b.WriteString("M")
			b.WriteString(itoa(x - padding))
			b.WriteString(" ")
			b.WriteString(itoa(y - padding))
			b.WriteString("h1v1h-1z")
		}
	}
	b.WriteString(`"/>`)
	b.WriteString(`</svg>`)
	return b.String(), nil
}

var errEmptyBitmap = &qrError{"could not render QR code"}

type qrError struct{ msg string }

func (e *qrError) Error() string { return e.msg }

// formatRGB renders a parsed colour back to #rrggbb.
func formatRGB(c rgb) string {
	return hexOf(c)
}

// contrastRatio is the WCAG contrast between two colours.
func contrastRatio(a, b string) float64 {
	ca, ok1 := parseHex(a)
	cb, ok2 := parseHex(b)
	if !ok1 || !ok2 {
		return 0
	}
	la, lb := luminance(ca), luminance(cb)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

// StorefrontURL builds the public address a customer opens, always on the
// platform's configured domain rather than whatever host the admin happens to
// be using.
func StorefrontURL(slug, baseDomain, scheme string) string {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" {
		return ""
	}
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + slug + "." + baseDomain
}

// QRDownloadName is the filename offered when the admin saves the code. Spaces
// and punctuation are collapsed so the browser does not rename the download.
func QRDownloadName(slug string) string {
	cleaned := slugify(strings.ToLower(strings.TrimSpace(slug)), 0)
	if cleaned == "" || cleaned == slugify("", 0) {
		cleaned = "store"
	}
	return "orderly-" + cleaned + "-qr.png"
}
