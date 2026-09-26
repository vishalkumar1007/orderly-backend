package orders

import (
	"math"
	"testing"
)

func sampleProduct() ProductRef {
	return ProductRef{
		ID:          "p1",
		Name:        "Veg Momo",
		Price:       120,
		Available:   true,
		AllowsNotes: true,
		Addons: []AddonRef{
			{ID: "spicy", Name: "Extra spicy", Price: 10, MaxQty: 1},
			{ID: "sauce", Name: "Extra sauce", Price: 20, MaxQty: 2},
		},
	}
}

func TestPriceCartUsesServerPrices(t *testing.T) {
	products := map[string]ProductRef{"p1": sampleProduct()}
	lines := []RequestedLine{{ProductID: "p1", Quantity: 2}}

	priced, totals, err := PriceCart(products, lines, Costing{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(priced) != 1 {
		t.Fatalf("expected 1 priced line, got %d", len(priced))
	}
	// 2 × 120 with no add-ons.
	if totals.Subtotal != 240 {
		t.Errorf("subtotal = %v, want 240", totals.Subtotal)
	}
	if totals.Total != 240 {
		t.Errorf("total = %v, want 240", totals.Total)
	}
	if priced[0].UnitTotal != 120 {
		t.Errorf("unit total = %v, want 120", priced[0].UnitTotal)
	}
}

func TestPriceCartAppliesAddonsFromTheProduct(t *testing.T) {
	products := map[string]ProductRef{"p1": sampleProduct()}
	lines := []RequestedLine{{
		ProductID: "p1",
		Quantity:  2,
		// A client-supplied price on an add-on must be ignored entirely: the
		// server looks the price up by id.
		Addons: []RequestedAddon{{ID: "spicy", Quantity: 1}, {ID: "sauce", Quantity: 2}},
	}}

	priced, totals, err := PriceCart(products, lines, Costing{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Per item: 120 + 10 + (20 × 2) = 170. Two items: 340.
	if priced[0].UnitTotal != 170 {
		t.Errorf("unit total = %v, want 170", priced[0].UnitTotal)
	}
	if totals.Total != 340 {
		t.Errorf("total = %v, want 340", totals.Total)
	}
}

func TestPriceCartRejectsUnknownAddon(t *testing.T) {
	products := map[string]ProductRef{"p1": sampleProduct()}
	lines := []RequestedLine{{ProductID: "p1", Quantity: 1, Addons: []RequestedAddon{{ID: "free_cheese", Quantity: 1}}}}

	_, _, err := PriceCart(products, lines, Costing{})
	if err == nil {
		t.Fatal("expected an unknown add-on to be rejected")
	}
	if err.Code != "unknown_addon" {
		t.Errorf("code = %q, want unknown_addon", err.Code)
	}
}

func TestPriceCartEnforcesAddonMaxQuantity(t *testing.T) {
	products := map[string]ProductRef{"p1": sampleProduct()}
	lines := []RequestedLine{{ProductID: "p1", Quantity: 1, Addons: []RequestedAddon{{ID: "spicy", Quantity: 5}}}}

	_, _, err := PriceCart(products, lines, Costing{})
	if err == nil || err.Code != "addon_limit" {
		t.Fatalf("expected addon_limit, got %v", err)
	}
}

func TestPriceCartRejectsUnavailableProduct(t *testing.T) {
	p := sampleProduct()
	p.Available = false
	products := map[string]ProductRef{"p1": p}

	_, _, err := PriceCart(products, []RequestedLine{{ProductID: "p1", Quantity: 1}}, Costing{})
	if err == nil || err.Code != "product_unavailable" {
		t.Fatalf("expected product_unavailable, got %v", err)
	}
}

func TestPriceCartRejectsUnknownProduct(t *testing.T) {
	_, _, err := PriceCart(map[string]ProductRef{}, []RequestedLine{{ProductID: "ghost", Quantity: 1}}, Costing{})
	if err == nil || err.Code != "product_unavailable" {
		t.Fatalf("expected product_unavailable, got %v", err)
	}
}

func TestPriceCartRejectsBadQuantities(t *testing.T) {
	products := map[string]ProductRef{"p1": sampleProduct()}
	cases := []struct {
		name string
		line RequestedLine
	}{
		{"zero", RequestedLine{ProductID: "p1", Quantity: 0}},
		{"negative", RequestedLine{ProductID: "p1", Quantity: -3}},
		{"over the per-line ceiling", RequestedLine{ProductID: "p1", Quantity: MaxQtyPerLine + 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := PriceCart(products, []RequestedLine{tc.line}, Costing{}); err == nil {
				t.Fatal("expected rejection")
			}
		})
	}
}

func TestPriceCartAppliesTaxAndPackaging(t *testing.T) {
	products := map[string]ProductRef{"p1": sampleProduct()}
	priced, totals, err := PriceCart(products,
		[]RequestedLine{{ProductID: "p1", Quantity: 2}},
		Costing{TaxPercent: 5, PackagingFee: 10})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 240 subtotal + 12 tax (5%) + 10 packaging.
	if totals.Subtotal != 240 {
		t.Errorf("subtotal = %v, want 240", totals.Subtotal)
	}
	if totals.Tax != 12 {
		t.Errorf("tax = %v, want 12", totals.Tax)
	}
	if totals.Packaging != 10 {
		t.Errorf("packaging = %v, want 10", totals.Packaging)
	}
	if totals.Total != 262 {
		t.Errorf("total = %v, want 262", totals.Total)
	}
	_ = priced
}

func TestPriceCartNeverReturnsNegativeTotal(t *testing.T) {
	products := map[string]ProductRef{"free": {ID: "free", Name: "Free sample", Price: 0, Available: true}}
	_, totals, err := PriceCart(products, []RequestedLine{{ProductID: "free", Quantity: 1}},
		Costing{PackagingFee: 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if totals.Total < 0 {
		t.Fatalf("total = %v, must not be negative", totals.Total)
	}
}

func TestPriceCartRejectsEmptyCart(t *testing.T) {
	if _, _, err := PriceCart(map[string]ProductRef{}, nil, Costing{}); err == nil {
		t.Fatal("expected an empty cart to be rejected")
	}
}

func TestPriceCartCapsTotalQuantity(t *testing.T) {
	products := map[string]ProductRef{"p1": sampleProduct()}
	// Twenty lines of ten each is 200 items, which is allowed; the next line
	// tips it over the ceiling.
	lines := make([]RequestedLine, 0, 25)
	for i := 0; i < 25; i++ {
		lines = append(lines, RequestedLine{ProductID: "p1", Quantity: 10})
	}
	if _, _, err := PriceCart(products, lines, Costing{}); err == nil {
		t.Fatal("expected an oversized order to be rejected")
	}
}

func TestSanitiseNotes(t *testing.T) {
	t.Run("strips control characters", func(t *testing.T) {
		got := sanitiseNotes("extra\x00 spicy\nplease")
		if got != "extra spicy please" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("truncates to the limit", func(t *testing.T) {
		long := ""
		for i := 0; i < MaxNotesLength+50; i++ {
			long += "x"
		}
		if got := sanitiseNotes(long); len([]rune(got)) != MaxNotesLength {
			t.Errorf("length = %d, want %d", len([]rune(got)), MaxNotesLength)
		}
	})
}

func TestNormalisePhoneInput(t *testing.T) {
	cases := map[string]string{
		"9876543210":        "+919876543210",
		"+91 98765 43210":   "+919876543210",
		"098765 43210":      "+919876543210",
		"+1 (555) 010-9999": "+15550109999",
		"  98765-43210  ":   "+919876543210",
		"":                  "",
	}
	for input, want := range cases {
		if got := normalisePhoneInput(input); got != want {
			t.Errorf("normalisePhoneInput(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLooksLikeEmail(t *testing.T) {
	valid := []string{"a@b.co", "customer.name+tag@sub.example.in"}
	invalid := []string{"", "plain", "@nolocal.com", "no@domain", "no@tld.", "has space@x.com"}
	for _, v := range valid {
		if !looksLikeEmail(v) {
			t.Errorf("looksLikeEmail(%q) = false, want true", v)
		}
	}
	for _, v := range invalid {
		if looksLikeEmail(v) {
			t.Errorf("looksLikeEmail(%q) = true, want false", v)
		}
	}
}

func TestMoneyRoundsToTwoDecimals(t *testing.T) {
	cases := map[float64]float64{
		0.1 + 0.2: 0.3,
		1.005:     1.01,
		2.344:     2.34,
		// Half away from zero, so a negative half-paisa rounds down, not toward
		// zero. The total is never negative anyway; this keeps the rule
		// symmetric so line items and totals can never disagree.
		-1.005: -1.01,
	}
	for in, want := range cases {
		if got := money(in); math.Abs(got-want) > 0.0001 {
			t.Errorf("money(%v) = %v, want %v", in, got, want)
		}
	}
}

func TestNormalisePhoneInputIsIdempotent(t *testing.T) {
	once := normalisePhoneInput("9876543210")
	if twice := normalisePhoneInput(once); twice != once {
		t.Errorf("normalising twice changed the value: %q then %q", once, twice)
	}
}
