package market

import (
	"context"
	"slices"
	"strings"
)

// privacy.go — B41.193: EVERY LISTING'S PRIVACY LABEL.
//
// Each version of a listing shows which models it calls, which tools it uses and what data it keeps, all read
// from its own artifact: "model" (a pipeline's steps each add theirs, as a use would run them without a buyer's
// model), "tools" and "keeps" — lists of names its seller gives at publish. A use stores no input or output for
// the seller, so a version that names nothing it keeps keeps nothing.

// Privacy is a version's privacy label.
type Privacy struct {
	Models       []string `json:"models"`        // every model it calls, in order, once each
	Tools        []string `json:"tools"`         // every tool it uses
	Keeps        []string `json:"keeps"`         // the data it keeps
	KeepsNothing bool     `json:"keeps_nothing"` // Keeps is empty
}

// checkPrivacy refuses an artifact whose "tools" or "keeps" is not a list of names, which the label could not show.
func checkPrivacy(obj map[string]any) error {
	for _, field := range []string{"tools", "keeps"} {
		v, given := obj[field]
		if !given {
			continue
		}
		list, ok := v.([]any)
		if !ok {
			return invalid("an artifact's %q must be a list of names", field)
		}
		for _, x := range list {
			if s, ok := x.(string); !ok || strings.TrimSpace(s) == "" {
				return invalid("an artifact's %q must be a list of names", field)
			}
		}
	}
	return nil
}

// privacyOf reads the label of l's version whose artifact is a. A pipeline's steps are read as its seller sees them,
// as a use runs them; a step that no longer resolves cannot run, and adds nothing.
func (s *Store) privacyOf(ctx context.Context, l Listing, a map[string]any) Privacy {
	p := Privacy{Models: []string{}, Tools: []string{}, Keeps: []string{}}
	add := func(to *[]string, names ...string) {
		for _, n := range names {
			if n = strings.TrimSpace(n); n != "" && !slices.Contains(*to, n) {
				*to = append(*to, n)
			}
		}
	}
	own := func(a map[string]any) {
		add(&p.Tools, names(a["tools"])...)
		add(&p.Keeps, names(a["keeps"])...)
	}
	model, _ := a["model"].(string)
	own(a)
	if l.Kind != "pipeline" {
		add(&p.Models, model)
	} else {
		steps, _ := a["steps"].([]any)
		for _, r := range steps {
			ref, _ := r.(map[string]any)
			use, _ := ref["use"].(string)
			id := use
			if _, after, named := strings.Cut(use, ":"); named {
				id = after
			}
			version, _ := ref["version"].(float64)
			_, sa, _, err := s.resolve(ctx, l.WorkspaceID, id, int(version))
			if err != nil {
				continue
			}
			stepModel, _ := sa["model"].(string)
			if stepModel == "" {
				stepModel = model
			}
			add(&p.Models, stepModel)
			own(sa)
		}
	}
	p.KeepsNothing = len(p.Keeps) == 0
	return p
}

func names(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
