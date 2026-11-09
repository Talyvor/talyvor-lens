package main

import (
	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
)

func mountKeyRecoveryRoutes(r chi.Router, bank *economy.DualTokenStore) {}
