package economy

import (
	"encoding/json"
	"os"
	"testing"
)

// B61.101 — every picture the gallery was drawn with (a copy of design/agent-pictures/pictures.json) is one an agent
// can wear, in the colour it was drawn in.
func TestAgentPictures_HoldTheDrawnGallery(t *testing.T) {
	raw, err := os.ReadFile("testdata/agent-pictures.json")
	if err != nil {
		t.Fatal(err)
	}
	var drawn struct {
		Pictures []AgentPicture `json:"pictures"`
	}
	if err := json.Unmarshal(raw, &drawn); err != nil {
		t.Fatal(err)
	}
	if len(drawn.Pictures) != 20 {
		t.Fatalf("the copy lists %d pictures, want 20", len(drawn.Pictures))
	}
	for _, want := range drawn.Pictures {
		if got, err := pictureColour(want.ID, ""); err != nil || got != want.Colour {
			t.Errorf("picture %q = %q, %v; want colour %q", want.ID, got, err, want.Colour)
		}
	}
}
