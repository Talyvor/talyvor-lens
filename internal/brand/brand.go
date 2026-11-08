// Package brand carries the Talyvor brand for the pages Lens renders itself:
// Pocket's colour tokens and type (design/pocket/pocket.css, B57.14) and the mark.
//
// The two SVGs beside this file are byte-for-byte copies of
// brand-v4/svg/talyvor-mark-flat-{dark,light}.svg. Never redraw or recolour
// them; replace the files instead (B57.81 swaps them for the chosen logo).
//
// Nothing here requests a font: Bricolage Grotesque and Figtree are named first
// and fall back to the system stack, so an API host never makes a visitor's
// browser talk to a third party, and pages whose policy is default-src 'none'
// still render.
package brand

import (
	_ "embed"
	"strings"
)

//go:embed talyvor-mark-flat-dark.svg
var markFlatDark string

//go:embed talyvor-mark-flat-light.svg
var markFlatLight string

// Mark is the flat mark for inline use: the dark-theme file and the light-theme
// file, each shown only in its own theme by TokensCSS. It is decorative beside
// the product name, so it is hidden from assistive technology.
var Mark = `<span class="tv-mark" aria-hidden="true">` +
	`<span class="tv-mark-dark">` + inline(markFlatDark) + `</span>` +
	`<span class="tv-mark-light">` + inline(markFlatLight) + `</span>` +
	`</span>`

// MarkLightSVG is the light-theme flat mark file as it is, for documents drawn on paper (B32.40's receipt PDF).
func MarkLightSVG() string { return markFlatLight }

// inline drops the namespace declaration, which HTML supplies for an inline
// <svg> anyway, so a page carrying the mark contains no http:// URL at all. The
// drawing is untouched.
func inline(svg string) string {
	return strings.Replace(strings.TrimSpace(svg), ` xmlns="http://www.w3.org/2000/svg"`, "", 1)
}

// TokensCSS is Pocket's tokens under the names these pages already use, with
// the theme switch driven by the viewer's preference: light is primary, dark
// under prefers-color-scheme, as in pocket.css. Figures are drawn in the UI
// face with tabular digits; --tv-font-mono is only for identifiers.
const TokensCSS = `:root{
` + lightTokens + `  --tv-font-display:"Bricolage Grotesque","Figtree","Helvetica Neue",Arial,sans-serif;
  --tv-font-sans:"Figtree","Helvetica Neue",Arial,sans-serif;
  --tv-font-mono:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
  --tv-radius-sm:12px; --tv-radius-md:24px; --tv-radius-pill:999px;
}
@media (prefers-color-scheme: dark){
  :root{
    --tv-canvas:#0F1013; --tv-surface:#181A1F; --tv-raised:#20232A;
    --tv-line:#2A2E36; --tv-line-strong:#6E7482;
    --tv-ink:#F2F3F5; --tv-ink-muted:#A6ACB9; --tv-label:#8A90A0;
    --tv-accent:#FF7A1A; --tv-accent-tint:#3A2414;
    --tv-positive:#3DD68C; --tv-caution:#FF8A33; --tv-critical:#FF6B6B;
    color-scheme:dark;
  }
}
h1,h2,h3{font-family:var(--tv-font-display)}
.tv-mark{display:inline-flex; flex:none}
.tv-mark-dark,.tv-mark-light{display:block; width:100%; height:100%}
.tv-mark-dark{display:none}
.tv-mark svg{display:block; width:100%; height:100%}
@media (prefers-color-scheme: dark){
  .tv-mark-light{display:none} .tv-mark-dark{display:block}
}
.tv-rule{display:block; width:32px; height:2px; background:var(--tv-accent); border:0; margin:0}
`

// PrintCSS puts a page in the light theme on paper whatever the screen's
// preference: browsers drop backgrounds when printing, so the dark theme would
// print light text on white. Add it after TokensCSS on a page meant to be
// printed.
const PrintCSS = `@media print{
  :root{
` + lightTokens + `  }
  .tv-mark-dark{display:none} .tv-mark-light{display:block}
}
`

// lightTokens is pocket.css's light column (.pk-light): page, cloud and
// cloud-2 for canvas, surface and raised; tangerine is the accent, never text.
const lightTokens = `  --tv-canvas:#FFFFFF; --tv-surface:#F3F4F6; --tv-raised:#ECEEF2;
  --tv-line:#E4E6EB; --tv-line-strong:#7A8192;
  --tv-ink:#111216; --tv-ink-muted:#4A5061; --tv-label:#646B7B;
  --tv-accent:#FF7A1A; --tv-accent-tint:#FFF1E6;
  --tv-positive:#0B7A45; --tv-caution:#A84100; --tv-critical:#C0262D;
  color-scheme:light;
`
