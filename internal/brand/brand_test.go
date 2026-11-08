package brand

import (
	"bytes"
	"image/png"
	"strings"
	"testing"
)

// B57.81 — the mark every Lens page draws is the chosen logo's symbol, light and dark, and none of the flat mark it
// replaced; the PNG for documents is the light symbol.
func TestMark_IsTheChosenSymbol(t *testing.T) {
	for _, want := range []string{`id="tslf-top"`, `id="tsdf-top"`, `class="tv-mark-light"`, `class="tv-mark-dark"`} {
		if !strings.Contains(Mark, want) {
			t.Errorf("the mark is missing %s", want)
		}
	}
	for _, old := range []string{"Talyvor mark", "#3AD6C0", "#060A12", "talyvor-mark-flat"} {
		if strings.Contains(Mark, old) {
			t.Errorf("the mark still carries today's %s", old)
		}
	}
	img, err := png.Decode(bytes.NewReader(MarkLightPNG))
	if err != nil {
		t.Fatalf("MarkLightPNG: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 256 || b.Dy() != 184 {
		t.Errorf("MarkLightPNG is %dx%d, want the kit's 256x184", b.Dx(), b.Dy())
	}
}
