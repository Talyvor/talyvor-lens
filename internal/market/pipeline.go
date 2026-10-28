package market

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// pipeline.go — B20.7: USE A PIPELINE LISTING.
//
// A pipeline's artifact is {"steps": [{"use": "listing:<listing id>", "version": 2}, …], "model": "…"}. Each
// step names a listing by its id, in the "<kind>:<ref>" form B20.4's review reads — "listing:lst_…", or
// "prompt:lst_…" to say what it must be — the pipeline seller's own listing or a public one, and optionally
// the version to run (the latest otherwise). A step is an agent, a prompt or a skill that has passed
// review; a pipeline cannot run an evaluation or another pipeline.
//
// The steps run in order through the same runner as any use: the buyer's input is the first step's input,
// and each step's output is the next one's. A prompt step's variables the buyer did not give are filled with
// its input. Each step runs on the buyer's model if they named one, else the step's own, else the
// pipeline's.
//
// The pipeline is billed as ONE use at its own price, which covers every step that is its seller's own
// listing. A step that is ANOTHER seller's listing is also a use of that listing — its own row, charged to
// the buyer as a direct use would be (billed, free, or nothing for a linked seller) and metered on its own —
// so a pipeline cannot resell another seller's work without paying them.

// maxPipelineSteps bounds how many model calls one use of a pipeline makes.
const maxPipelineSteps = 10

// pipelineStep is one step, resolved before the pipeline runs.
type pipelineStep struct {
	StepResult
	seller    string
	artifact  map[string]any
	charge    string // what chargeFor said it costs, before any trial (B32.21)
	priceULXC int64
	trialULXC int64 // a trial's would-be price
}

// pipelineSteps resolves every step of pipeline l for buyer, and what each costs them.
func (s *Store) pipelineSteps(ctx context.Context, l Listing, artifact map[string]any, buyer string, req UseRequest) ([]pipelineStep, error) {
	raw, _ := artifact["steps"].([]any)
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: the pipeline has no steps", ErrInvalid)
	}
	if len(raw) > maxPipelineSteps {
		return nil, fmt.Errorf("%w: a pipeline of more than %d steps cannot be run in one use", ErrInvalid, maxPipelineSteps)
	}
	pipelineModel, _ := artifact["model"].(string)
	var steps []pipelineStep
	for i, r := range raw {
		n := i + 1
		ref, _ := r.(map[string]any)
		use, _ := ref["use"].(string)
		kind, id, named := strings.Cut(use, ":")
		if !named {
			kind, id = "listing", use
		}
		version := 0
		if v, ok := ref["version"].(float64); ok {
			version = int(v)
		}
		// Seen as the pipeline's seller sees it: their own private listings may be steps, nobody else's.
		sl, sa, sv, err := s.resolve(ctx, l.WorkspaceID, id, version)
		switch {
		case errors.Is(err, ErrNotFound), errors.Is(err, ErrTakenDown):
			return nil, fmt.Errorf(`%w: step %d names no listing this pipeline can run (%q) — a step's "use" is "listing:<a listing's id>"`, ErrInvalid, n, use)
		case err != nil:
			return nil, err
		case sl.ReviewStatus != ReviewApproved:
			return nil, fmt.Errorf("%w: step %d's listing is waiting for review", ErrInvalid, n)
		case sl.Kind != "agent" && sl.Kind != "prompt" && sl.Kind != "skill":
			return nil, fmt.Errorf("%w: step %d is a %s, and a pipeline's steps are agents, prompts or skills", ErrInvalid, n, sl.Kind)
		case kind != "listing" && kind != sl.Kind:
			return nil, fmt.Errorf("%w: step %d names a %s, but %s is a %s", ErrInvalid, n, kind, id, sl.Kind)
		}
		st := pipelineStep{StepResult: StepResult{Step: n, ListingID: sl.ID, Version: sv, Kind: sl.Kind}, seller: sl.WorkspaceID, artifact: sa}
		if st.Model = req.Model; st.Model == "" {
			if st.Model, _ = sa["model"].(string); st.Model == "" {
				st.Model = pipelineModel
			}
		}
		if st.Model == "" {
			return nil, fmt.Errorf("%w (step %d)", ErrNoModel, n)
		}
		if sl.WorkspaceID != l.WorkspaceID {
			st.UseID = "use_" + uuid.NewString()
			if st.charge, st.priceULXC, err = s.chargeFor(ctx, sl, buyer); err != nil {
				return nil, err
			}
			st.Charge, st.PriceULXC = st.charge, st.priceULXC
		}
		steps = append(steps, st)
	}
	return steps, nil
}

// runPipeline runs the steps in order, each on the one before's output, and answers with the last's.
func runPipeline(ctx context.Context, r Runner, steps []pipelineStep, req UseRequest, u *Use) error {
	input := req.Input
	for _, st := range steps {
		vars := map[string]string{}
		for k, v := range req.Variables {
			vars[k] = v
		}
		if st.Kind == "prompt" && strings.TrimSpace(input) != "" {
			t, _ := st.artifact["template"].(string)
			for _, m := range variable.FindAllStringSubmatch(t, -1) {
				if _, given := vars[m[1]]; !given {
					vars[m[1]] = input
				}
			}
		}
		calls, _, err := plan(st.Kind, st.artifact, UseRequest{Model: st.Model, Input: input, Variables: vars})
		if err != nil {
			return fmt.Errorf("step %d: %w", st.Step, err)
		}
		out, err := answer(ctx, r, st.Model, calls[0].messages)
		if err != nil {
			return err
		}
		st.Output, input = out, out
		u.Steps = append(u.Steps, st.StepResult)
	}
	u.Output, u.Model = input, steps[len(steps)-1].Model
	return nil
}
