package orders

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf8"
)

// RequestedAddon is what the client asks for: an id and a count, never a price.
type RequestedAddon struct {
	ID       string `json:"id"`
	Quantity int    `json:"quantity"`
}

// RequestedLine is one cart row as submitted.
type RequestedLine struct {
	ProductID string           `json:"product_id"`
	Quantity  int              `json:"quantity"`
	Addons    []RequestedAddon `json:"addons"`
	Notes     string           `json:"notes"`
}

// LineAddon is a priced add-on on a cart line.
type LineAddon struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Price    float64 `json:"price"`
	Quantity int     `json:"quantity"`
}

// PricedLine is a cart row after the backend has resolved prices.
type PricedLine struct {
	Product    ProductRef
	Quantity   int
	Addons     []LineAddon
	Notes      string
	UnitBase   float64
	UnitAddons float64
	UnitTotal  float64
	LineTotal  float64
}

// ProductRef is the slice of a product the pricing step needs.
type ProductRef struct {
	ID          string
	Name        string
	Price       float64
	Available   bool
	AllowsNotes bool
	Addons      []AddonRef
}

// AddonRef is a product's add-on with its server-side price.
type AddonRef struct {
	ID     string
	Name   string
	Price  float64
	MaxQty int
}

// Totals is the full money breakdown. Discount is always present so the cart
// and checkout can render the row without special-casing its absence.
type Totals struct {
	Subtotal  float64 `json:"subtotal"`
	Tax       float64 `json:"tax"`
	Packaging float64 `json:"packaging_fee"`
	Discount  float64 `json:"discount"`
	Total     float64 `json:"total"`
}

// Costing is the tenant's pricing inputs for an order.
type Costing struct {
	TaxPercent   float64
	PackagingFee float64
}

// Cart limits. They exist to stop a crafted payload from creating an
// unreasonable order, not to police normal customer behaviour.
const (
	MaxLinesPerOrder   = 40
	MaxQtyPerLine      = 50
	MaxNotesLength     = 240
	MaxTotalQuantity   = 200
	maxPerLineQuantity = 99
)

// PriceError is a customer-presentable pricing or validation failure.
type PriceError struct {
	Code    string
	Message string
}

func (e *PriceError) Error() string { return e.Message }

func priceErr(code, format string, args ...any) *PriceError {
	return &PriceError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// PriceCart validates a submitted cart against server-side product data and
// returns priced lines plus totals.
//
// This is the only place order money is calculated. The client sends product
// ids, quantities and add-on ids; names, prices and totals all come from the
// products loaded out of the database, so a tampered cart cannot change what
// the customer is charged.
func PriceCart(products map[string]ProductRef, lines []RequestedLine, costing Costing) ([]PricedLine, Totals, *PriceError) {
	if len(lines) == 0 {
		return nil, Totals{}, priceErr("empty_cart", "your cart is empty")
	}
	if len(lines) > MaxLinesPerOrder {
		return nil, Totals{}, priceErr("cart_too_large", "too many different items in one order")
	}

	priced := make([]PricedLine, 0, len(lines))
	totalQuantity := 0
	var subtotal float64

	for _, line := range lines {
		if line.Quantity < 1 || line.Quantity > MaxQtyPerLine {
			return nil, Totals{}, priceErr("invalid_quantity", "choose a quantity between 1 and %d", MaxQtyPerLine)
		}
		product, ok := products[line.ProductID]
		if !ok {
			return nil, Totals{}, priceErr("product_unavailable", "one of the items in your cart is no longer available")
		}
		if !product.Available {
			return nil, Totals{}, priceErr("product_unavailable", "%s is sold out right now", product.Name)
		}

		addons, addonsTotal, perr := priceAddons(product, line.Addons)
		if perr != nil {
			return nil, Totals{}, perr
		}

		notes := strings.TrimSpace(line.Notes)
		if notes != "" {
			if !product.AllowsNotes {
				// The product does not accept special instructions; drop them
				// rather than failing an otherwise good order.
				notes = ""
			} else {
				notes = sanitiseNotes(notes)
			}
		}

		unit := money(product.Price + addonsTotal)
		lineTotal := money(unit * float64(line.Quantity))

		priced = append(priced, PricedLine{
			Product:    product,
			Quantity:   line.Quantity,
			Addons:     addons,
			Notes:      notes,
			UnitBase:   money(product.Price),
			UnitAddons: money(addonsTotal),
			UnitTotal:  unit,
			LineTotal:  lineTotal,
		})
		subtotal += lineTotal
		totalQuantity += line.Quantity
		if totalQuantity > MaxTotalQuantity {
			return nil, Totals{}, priceErr("cart_too_large", "that is more food than one order can hold — please split it")
		}
	}

	subtotal = money(subtotal)
	totals := Totals{
		Subtotal:  subtotal,
		Tax:       money(subtotal * clampPercent(costing.TaxPercent) / 100),
		Packaging: money(nonNegative(costing.PackagingFee)),
	}
	totals.Total = money(subtotal + totals.Tax + totals.Packaging - totals.Discount)
	if totals.Total < 0 {
		totals.Total = 0
	}
	return priced, totals, nil
}

// priceAddons resolves requested add-ons against the product's catalogue. An
// unknown id is an error, not a silent drop: the customer asked for something
// and the server must not quietly charge less than they expected.
func priceAddons(product ProductRef, requested []RequestedAddon) ([]LineAddon, float64, *PriceError) {
	if len(requested) == 0 {
		return nil, 0, nil
	}
	if len(requested) > 12 {
		return nil, 0, priceErr("too_many_addons", "too many extra options on one item")
	}
	seen := map[string]bool{}
	out := make([]LineAddon, 0, len(requested))
	var total float64

	for _, want := range requested {
		id := strings.TrimSpace(want.ID)
		if id == "" {
			return nil, 0, priceErr("invalid_addon", "one of the extra options is invalid")
		}
		if seen[id] {
			return nil, 0, priceErr("invalid_addon", "the same extra option was listed twice")
		}
		seen[id] = true
		if want.Quantity < 1 || want.Quantity > maxPerLineQuantity {
			return nil, 0, priceErr("invalid_addon_quantity", "invalid quantity for an extra option")
		}
		var match *AddonRef
		for i := range product.Addons {
			if product.Addons[i].ID == id {
				match = &product.Addons[i]
				break
			}
		}
		if match == nil {
			return nil, 0, priceErr("unknown_addon", "an extra option is no longer available")
		}
		qty := want.Quantity
		if match.MaxQty > 0 && qty > match.MaxQty {
			return nil, 0, priceErr("addon_limit", "you can add at most %d × %s", match.MaxQty, match.Name)
		}
		out = append(out, LineAddon{ID: match.ID, Name: match.Name, Price: match.Price, Quantity: qty})
		total += match.Price * float64(qty)
	}
	return out, total, nil
}

func sanitiseNotes(notes string) string {
	notes = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if r < 0x20 {
			return -1
		}
		return r
	}, notes)
	notes = strings.Join(strings.Fields(notes), " ")
	if utf8.RuneCountInString(notes) > MaxNotesLength {
		runes := []rune(notes)
		notes = string(runes[:MaxNotesLength])
	}
	return notes
}

func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func nonNegative(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

// money rounds to two decimals, half away from zero. Every amount that leaves
// this package passes through here, so float drift can never accumulate into a
// total that disagrees with the sum of its lines.
//
// The rounding is done on the decimal value the number represents rather than
// on its binary approximation: 1.005 is stored as 1.00499999…, so a naive
// math.Round(v*100) would silently swallow the half paisa. Parsing the shortest
// decimal form and rounding that in exact integer arithmetic is both correct
// and cheap, because it runs once per line and once per total — not per
// operation.
func money(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	rat, ok := new(big.Rat).SetString(strconv.FormatFloat(v, 'f', -1, 64))
	if !ok {
		return 0
	}
	scaled := new(big.Rat).Mul(rat, big.NewRat(100, 1))
	quotient, remainder := new(big.Int).QuoRem(scaled.Num(), scaled.Denom(), new(big.Int))
	// Round half away from zero: the remainder is at least half when twice its
	// magnitude reaches the denominator.
	twiceRem := new(big.Int).Abs(remainder)
	twiceRem.Lsh(twiceRem, 1)
	if twiceRem.Cmp(scaled.Denom()) >= 0 {
		if scaled.Sign() < 0 {
			quotient.Sub(quotient, big.NewInt(1))
		} else {
			quotient.Add(quotient, big.NewInt(1))
		}
	}
	out, _ := new(big.Rat).SetFrac(quotient, big.NewInt(100)).Float64()
	return out
}
