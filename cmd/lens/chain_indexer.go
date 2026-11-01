package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/talyvor/lens/internal/economy"
	"github.com/talyvor/lens/internal/money"
	"github.com/talyvor/lens/internal/money/coinbase"
	"github.com/talyvor/lens/internal/money/smartaccount"
)

// chain_indexer.go — B48.6: every movement of an agent's crypto wallet seen, confirmed and reconciled.
//
//	POST /v1/money/chain/webhooks/coinbase   the wallet provider's signed onchain-activity webhooks: each names a
//	                                         transaction, which Lens reads from its own node (economy.ChainIndexer)
//
// The leader polls each network's logs and confirms what it has seen every minute, and reconciles every wallet once a
// day, raising any difference to the operator. LENS_CHAIN_INDEX switches it on, network by network; unset, the
// webhook answers 503 and nothing runs.

// chainIndexConfig is LENS_CHAIN_INDEX: the provider's webhook secret and each network the indexer reads.
type chainIndexConfig struct {
	WebhookSecret string `json:"webhook_secret"`
	Networks      []struct {
		economy.ChainNetwork
		ChainID        int64  `json:"chain_id"`
		RPCURL         string `json:"rpc_url"`
		WebhookNetwork string `json:"webhook_network"` // the provider's name for it in webhooks, such as base-mainnet
	} `json:"networks"`
}

// chainIndex is the indexer and the provider's names for its networks.
type chainIndex struct {
	x             *economy.ChainIndexer
	webhookSecret string
	byWebhook     map[string]string
}

// newChainIndex reads LENS_CHAIN_INDEX's raw JSON; nil when it is unset.
func newChainIndex(store *economy.DualTokenStore, raw string) (*chainIndex, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var cfg chainIndexConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, fmt.Errorf("LENS_CHAIN_INDEX: %w", err)
	}
	nets := make([]economy.ChainNetwork, 0, len(cfg.Networks))
	byWebhook := map[string]string{}
	for _, n := range cfg.Networks {
		n.Chain = smartaccount.Chain{ID: n.ChainID, RPCURL: n.RPCURL}
		nets = append(nets, n.ChainNetwork)
		if n.WebhookNetwork != "" {
			byWebhook[n.WebhookNetwork] = n.Name
		}
	}
	x, err := store.NewChainIndexer(nets)
	if err != nil {
		return nil, fmt.Errorf("LENS_CHAIN_INDEX: %w", err)
	}
	return &chainIndex{x: x, webhookSecret: cfg.WebhookSecret, byWebhook: byWebhook}, nil
}

// serveWebhook is POST /v1/money/chain/webhooks/coinbase. A notification that does not verify is 401; one about a
// network or an event Lens does not read is taken and does nothing; a transaction the node does not have yet is 503,
// so the provider sends it again.
func (c *chainIndex) serveWebhook(w http.ResponseWriter, r *http.Request) {
	if c == nil || c.webhookSecret == "" {
		http.Error(w, "the chain indexer is not switched on", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return
	}
	a, err := coinbase.VerifyActivity(c.webhookSecret, r.Header, body, time.Now())
	if errors.Is(err, money.ErrBadSignature) {
		http.Error(w, "signature does not verify", http.StatusUnauthorized)
		return
	}
	if err != nil {
		http.Error(w, "the notification is unreadable", http.StatusBadRequest)
		return
	}
	network, ok := c.byWebhook[a.Data.NetworkID]
	if a.Type != "onchain.activity.detected" || a.Data.EventName != "Transfer" || !ok || a.Data.TransactionHash == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	if _, err := c.x.Observe(r.Context(), network, a.Data.TransactionHash, "webhook"); err != nil {
		slog.Warn("chain indexer: a webhook's transaction was not read; the provider sends it again", "network", network,
			"tx", a.Data.TransactionHash, "err", err)
		http.Error(w, "try again", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// indexChains polls and confirms every network each minute, and reconciles every wallet once a day.
func (c *chainIndex) indexChains(ctx context.Context, sink operatorAlerter) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	reconciled := ""
	for {
		for _, network := range c.x.Networks() {
			if _, err := c.x.Poll(ctx, network); err != nil {
				slog.Warn("chain indexer: the node's logs were not read", "network", network, "err", err)
			}
			if _, err := c.x.Confirm(ctx, network); err != nil {
				slog.Warn("chain indexer: movements were not confirmed", "network", network, "err", err)
			}
		}
		if day := time.Now().UTC().Format(time.DateOnly); day != reconciled {
			if err := c.reconcileOnce(ctx, sink); err != nil {
				slog.Warn("chain indexer: the nightly reconciliation did not complete", "err", err)
			} else {
				reconciled = day
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// reconcileOnce reconciles every wallet on every network and raises any difference to the operator.
func (c *chainIndex) reconcileOnce(ctx context.Context, sink operatorAlerter) error {
	var found []string
	var errs []error
	for _, network := range c.x.Networks() {
		diffs, err := c.x.Reconcile(ctx, network)
		errs = append(errs, err)
		for _, d := range diffs {
			found = append(found, fmt.Sprintf("%s %s (agent %s): the chain holds %d, the ledger %d and %d pending, a difference of %d micro-USDC",
				d.Network, d.Wallet, d.AgentID, d.ChainMinor, d.LedgerMinor, d.PendingMinor, d.DifferenceMinor))
		}
	}
	if len(found) > 0 {
		summary := strings.Join(found, "; ")
		slog.Error("chain indexer: agents' wallets on the chain and the ledger disagree", "differences", summary)
		if sink == nil {
			slog.Error("chain indexer: no operator alert sink is configured (LENS_OPERATOR_ALERT_WEBHOOK_URL and LENS_OPERATOR_ALERT_WEBHOOK_SECRET); nobody is told")
		} else if err := sink.NotifyAs(ctx, "chain_reconciliation_break", "lens/internal/economy",
			fmt.Sprintf("%d agent wallets differ from the ledger", len(found)), "Money moved on the chain that the ledger does not show — "+summary); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
