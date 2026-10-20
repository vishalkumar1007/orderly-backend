package storefront

import (
	"testing"

	"github.com/google/uuid"
)

func TestThemeParamsSurface(t *testing.T) {
	ok := " Paper "
	params, err := themeParams(themeRequest{Surface: &ok}, uuid.New())
	if err != nil || params.SurfaceStyle.String != "paper" {
		t.Fatalf("paper: got %q, %v", params.SurfaceStyle.String, err)
	}
	bad := "neon"
	if _, err := themeParams(themeRequest{Surface: &bad}, uuid.New()); err == nil {
		t.Fatal("neon: want error")
	}
}
