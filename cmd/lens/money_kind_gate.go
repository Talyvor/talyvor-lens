package main

import (
	"context"
	"net/http"
	"regexp"
)

// money_kind_gate.go — B59.7 red first: the table and the gate are not built yet.

type moneySide string

const (
	moneySideFiat    moneySide = "fiat"
	moneySideOnChain moneySide = "on_chain"
	moneySideEither  moneySide = "either"
)

var moneyRoutes = map[string]moneySide{}

type moneyKindReader interface {
	WorkspaceMoneyKind(ctx context.Context, workspaceID string) (string, error)
}

func moneyKindGate(kinds moneyKindReader) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler { return next }
}

// blankRouteParams writes every {name} in a chi pattern as {}.
func blankRouteParams(pattern string) string { return routeParam.ReplaceAllString(pattern, "{}") }

var routeParam = regexp.MustCompile(`\{[^}]*\}`)
