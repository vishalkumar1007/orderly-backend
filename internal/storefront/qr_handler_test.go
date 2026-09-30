package storefront

import "testing"

func TestStoreLinkHostAndPath(t *testing.T) {
	host, path := storeLinkHostAndPath("http://momo-magic.localhost:5173")
	if host != "momo-magic.localhost:5173" {
		t.Fatalf("host = %q", host)
	}
	if path != "/" {
		t.Fatalf("path = %q", path)
	}

	host, path = storeLinkHostAndPath("https://shop.example.com/menu")
	if host != "shop.example.com" {
		t.Fatalf("host = %q", host)
	}
	if path != "/menu" {
		t.Fatalf("path = %q", path)
	}
}
