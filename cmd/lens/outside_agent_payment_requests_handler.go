package main

import (
	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/agentsig"
	"github.com/talyvor/lens/internal/economy"
)

func mountOutsideAgentPaymentRoutes(r chi.Router, v *agentsig.Verifier, bank *economy.DualTokenStore) {}
