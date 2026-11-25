package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/talyvor/lens/internal/economy"
)

// B48.49 — the crypto addresses the owner saved and named, which an agent's send may name instead of the 0x address
// (economy/crypto_address_book.go).
//
//	GET    /v1/workspaces/{wsID}/crypto-addresses               every saved address, by name
//	POST   /v1/workspaces/{wsID}/crypto-addresses               {name, address} — saves it, or renames it when saved
//	DELETE /v1/workspaces/{wsID}/crypto-addresses/{addressID}   removes it
//
// Only the workspace's owner, signed in, reads or changes them. Mounted in the authed group.

type cryptoAddressBook interface {
	CryptoAddresses(ctx context.Context, workspaceID string) ([]economy.CryptoAddress, error)
	SaveCryptoAddress(ctx context.Context, workspaceID, by, name, address string) (economy.CryptoAddress, error)
	RemoveCryptoAddress(ctx context.Context, workspaceID, by, id string) error
}

func mountCryptoAddressBookRoutes(r chi.Router, book cryptoAddressBook) {
	owner := func(w http.ResponseWriter, req *http.Request) (string, bool) {
		by, ok := signedInOwner(req)
		if !ok {
			writeJSONErr(w, http.StatusForbidden, "only the workspace's owner, signed in, keeps its saved crypto addresses")
		}
		return by, ok
	}
	r.Get("/v1/workspaces/{wsID}/crypto-addresses", func(w http.ResponseWriter, req *http.Request) {
		if _, ok := owner(w, req); !ok {
			return
		}
		list, err := book.CryptoAddresses(req.Context(), chi.URLParam(req, "wsID"))
		if err != nil {
			writeCryptoAddressErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, map[string]any{"addresses": list})
	})
	r.Post("/v1/workspaces/{wsID}/crypto-addresses", func(w http.ResponseWriter, req *http.Request) {
		by, ok := owner(w, req)
		if !ok {
			return
		}
		var in struct{ Name, Address string }
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 2<<10)).Decode(&in); err != nil {
			writeJSONErr(w, http.StatusBadRequest, `body must be {"name": "<what you call it>", "address": "0x…"}`)
			return
		}
		a, err := book.SaveCryptoAddress(req.Context(), chi.URLParam(req, "wsID"), by, in.Name, in.Address)
		if err != nil {
			writeCryptoAddressErr(w, err)
			return
		}
		writeJSONOK(w, http.StatusOK, a)
	})
	r.Delete("/v1/workspaces/{wsID}/crypto-addresses/{addressID}", func(w http.ResponseWriter, req *http.Request) {
		by, ok := owner(w, req)
		if !ok {
			return
		}
		if err := book.RemoveCryptoAddress(req.Context(), chi.URLParam(req, "wsID"), by, chi.URLParam(req, "addressID")); err != nil {
			writeCryptoAddressErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func writeCryptoAddressErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, economy.ErrCryptoAddress):
		writeJSONErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, economy.ErrCryptoAddressNotFound):
		writeJSONErr(w, http.StatusNotFound, err.Error())
	default:
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
	}
}
