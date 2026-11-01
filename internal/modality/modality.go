// Package modality detects the modalities (text / image / audio / document)
// present in a chat request and knows which models can serve them, so the
// proxy can route a multimodal request to a CAPABLE model rather than
// silently flattening an image into text at a text-only model.
//
// Two design rules:
//
//   - Detection is STRUCTURAL and cheap: it inspects the content-block
//     `type` fields only. It never decodes the base64 image/audio bytes —
//     the minimal structs it unmarshals into don't even have the data
//     fields, so the JSON decoder skips them.
//   - Capability is CONSERVATIVE: a model whose capabilities are unknown is
//     treated as text-only. We never assume a model can do vision.
package modality

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/talyvor/lens/internal/catalog"
)

// Per-modality input-token estimates (documented, rough). Exact image-token
// accounting needs the rendered image dimensions or the provider's returned
// usage; until that's wired, multimodal input cost is an ESTIMATE built from
// the text length plus these per-item figures, and the spend row is flagged
// cost_estimated.
const (
	ImageTokenEstimate = 1000
	AudioTokenEstimate = 1000
)

// ModalitySet is what a request contains. TextChars is the total length of
// the text blocks only (image/audio bytes excluded), so callers can build a
// sane token estimate instead of counting the base64 blob.
type ModalitySet struct {
	HasImage    bool `json:"has_image"`
	HasAudio    bool `json:"has_audio"`
	HasDocument bool `json:"has_document"`
	ImageCount  int  `json:"image_count"`
	AudioCount  int  `json:"audio_count"`
	TextChars   int  `json:"text_chars"`
}

// Multimodal reports whether any non-text modality is present.
func (m ModalitySet) Multimodal() bool { return m.HasImage || m.HasAudio || m.HasDocument }

// Label is the canonical, low-cardinality modality string for metrics +
// the spend record: "text", or the comma-joined non-text modalities.
func (m ModalitySet) Label() string {
	var parts []string
	if m.HasImage {
		parts = append(parts, "image")
	}
	if m.HasAudio {
		parts = append(parts, "audio")
	}
	if m.HasDocument {
		parts = append(parts, "document")
	}
	if len(parts) == 0 {
		return "text"
	}
	return strings.Join(parts, ",")
}

// EstimateInputTokens returns a modality-aware input-token estimate: text
// chars / 4 plus the per-item estimates. Used for multimodal requests in
// place of len(rawBody)/4, which would count the base64 blob.
func (m ModalitySet) EstimateInputTokens() int {
	return m.TextChars/4 + m.ImageCount*ImageTokenEstimate + m.AudioCount*AudioTokenEstimate
}

// ─── detection ───

// detectShape captures only what detection needs — message content as raw
// JSON, decoded block-by-block below. No data/url fields, so base64 payloads
// are never decoded.
type detectShape struct {
	Messages []struct {
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// contentBlock is the union of the OpenAI + Anthropic block shapes, with ONLY
// the structural fields (type, text, source.type/media_type). The base64
// data fields (image_url.url, source.data, input_audio.data) are deliberately
// absent so they're skipped, never decoded.
type contentBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
	} `json:"source"`
}

// Detect inspects a chat-completions body and reports the modalities present.
// Recognises the OpenAI content-array shape (type "image_url" / "input_audio")
// and the Anthropic block shape (type "image" / "document" with a source).
// A parse failure yields an all-false set (treated as text) — detection must
// never break a request.
func Detect(body []byte) ModalitySet {
	var ms ModalitySet
	var req detectShape
	if err := json.Unmarshal(body, &req); err != nil {
		return ms
	}
	for _, m := range req.Messages {
		c := bytes.TrimSpace(m.Content)
		if len(c) == 0 {
			continue
		}
		// String content → plain text.
		if c[0] == '"' {
			var s string
			if json.Unmarshal(c, &s) == nil {
				ms.TextChars += len(s)
			}
			continue
		}
		// Array content → inspect each block's type (no data decode).
		if c[0] == '[' {
			var blocks []contentBlock
			if json.Unmarshal(c, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				switch b.Type {
				case "text":
					ms.TextChars += len(b.Text)
				case "image_url", "image":
					ms.HasImage = true
					ms.ImageCount++
				case "input_audio", "audio":
					ms.HasAudio = true
					ms.AudioCount++
				case "document", "file":
					ms.HasDocument = true
				}
			}
		}
	}
	return ms
}

// ─── capability registry ───

// Capabilities describes the non-text modalities a model can serve.
type Capabilities struct {
	Vision   bool `json:"vision"`
	Audio    bool `json:"audio"`
	Document bool `json:"document"`
}

// Capability FACTS now live in the model catalog (the single source of
// truth — Upgrade 16). modality reads them from there; an unlisted model is
// text-only (the conservative default), exactly as before.
func capsOf(model string) Capabilities {
	c := catalog.CapabilitiesOf(model)
	return Capabilities{Vision: c.Vision, Audio: c.Audio, Document: c.Document}
}

// providerPreference is the curated redirect-PREFERENCE order (a routing
// POLICY, like the router's tier ranks — kept here, not in the facts
// catalog). It deliberately lists the models we'd prefer to redirect a
// multimodal request to, cheapest-capable first; capability facts for each
// still come from the catalog via Supports. (Note it intentionally omits the
// nano tier as a redirect target even though it's vision-capable.)
var providerPreference = map[string][]string{
	"openai":    {"gpt-4o-mini", "gpt-4.1-mini", "gpt-4.1", "gpt-4o", "gpt-5.4-mini", "gpt-5.4"},
	"anthropic": {"claude-haiku-4-5", "claude-sonnet-4-6", "claude-opus-4-6", "claude-opus-4-5"}, // B38.11: never claude-sonnet-4-5, retiring
	"google":    {"gemini-2.5-flash", "gemini-2.0-flash", "gemini-1.5-flash", "gemini-2.5-pro", "gemini-1.5-pro"},
	"bedrock":   {"anthropic.claude-sonnet-4-6", "anthropic.claude-opus-4-6-v1"},
}

// Get returns a model's capabilities (zero value = text-only for unknowns).
func Get(model string) Capabilities { return capsOf(model) }

// Supports reports whether a model can serve every modality the request
// needs. A text-only request (all-false set) is supported by every model.
func Supports(model string, need ModalitySet) bool {
	c := capsOf(model)
	if need.HasImage && !c.Vision {
		return false
	}
	if need.HasAudio && !c.Audio {
		return false
	}
	if need.HasDocument && !c.Document {
		return false
	}
	return true
}

// CapableModel returns the preferred model of `provider` that serves `need`
// and is in the workspace's allowed set (empty allowed = all). Used to
// redirect an auto-route request whose chosen model can't handle the
// modality. Walks the curated providerPreference order (policy); capability
// facts come from the catalog via Supports. ok=false when none qualifies.
func CapableModel(provider string, need ModalitySet, allowed []string) (string, bool) {
	for _, m := range providerPreference[provider] {
		if len(allowed) > 0 && !contains(allowed, m) {
			continue
		}
		if Supports(m, need) {
			return m, true
		}
	}
	return "", false
}

// CapabilityMap returns the model→capabilities map for the introspection API,
// sourced from the catalog.
func CapabilityMap() map[string]Capabilities {
	all := catalog.All()
	out := make(map[string]Capabilities, len(all))
	for _, m := range all {
		out[m.ID] = Capabilities{Vision: m.Capabilities.Vision, Audio: m.Capabilities.Audio, Document: m.Capabilities.Document}
	}
	return out
}

// KnownModels returns the catalog's model ids, sorted (deterministic API).
func KnownModels() []string {
	all := catalog.All()
	out := make([]string, 0, len(all))
	for _, m := range all {
		out = append(out, m.ID)
	}
	sort.Strings(out)
	return out
}

func contains(hs []string, needle string) bool {
	for _, h := range hs {
		if h == needle {
			return true
		}
	}
	return false
}
