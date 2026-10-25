package main

import (
	"context"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/a2acard"
	"github.com/talyvor/lens/internal/ap2"
)

type approverCards interface {
	Card(ctx context.Context, agentID string) (a2acard.Card, error)
}

func mountAgentApproverRoutes(r chi.Router, bank agentBank, cards approverCards) {}

func mountAP2PublicRoutes(r chi.Router, signer *ap2.Signer) {}
