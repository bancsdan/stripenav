// stripenav is the bridge service: a small HTTP server that receives
// Stripe webhook events on /webhooks/stripe and reports the
// corresponding invoices to NAV's Online Számla v3.0 API.
//
// It's the same code that ships as the ghcr.io/bancsdan/stripenav
// Docker image. For embedding the bridge inside an existing server,
// see the library at https://github.com/bancsdan/go-stripenav.
//
// Stripe-side setup:
//  1. Create a webhook endpoint pointed at https://your-host/webhooks/stripe
//  2. Subscribe to: invoice.finalized, invoice.voided,
//     invoice.marked_uncollectible, credit_note.created, credit_note.voided
//  3. Put the endpoint signing secret (whsec_…) in STRIPE_WEBHOOK_SECRET.
//
// NAV-side setup:
//  1. Provision a "technical user" (műszaki felhasználó) on the NAV portal.
//  2. Set NAV_LOGIN, NAV_PASSWORD, NAV_TAX_NUMBER, NAV_SIGN_KEY, NAV_EXCHANGE_KEY.
//  3. Set NAV_BASE_URL to either nav.TestBaseURL or nav.ProductionBaseURL.
//
// Required env: see docs/DEPLOY.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	stripenav "github.com/bancsdan/go-stripenav"
	"github.com/bancsdan/go-stripenav/mapping"
	"github.com/bancsdan/go-stripenav/nav"
	"github.com/bancsdan/go-stripenav/storeinmem"
	"github.com/bancsdan/stripenav/internal/storefactory"
)

const shutdownGrace = 30 * time.Second

func main() {
	// --healthcheck: self-invoke via Docker HEALTHCHECK. The distroless
	// image has no shell or curl, so we use the binary itself: it hits
	// /healthz on the configured listen address and exits 0/1.
	if len(os.Args) > 1 && os.Args[1] == "--healthcheck" {
		os.Exit(runHealthcheck())
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	storeCtx, storeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	store, storeClose, err := storefactory.From(storeCtx, os.Getenv("STORE_URL"))
	storeCancel()
	if err != nil {
		logger.Error("store init", "err", err)
		os.Exit(1)
	}
	defer storeClose()

	if _, inMem := store.(*storeinmem.Store); inMem {
		logger.Warn("using in-memory submission store — data lost on restart; set STORE_URL=postgres://... for production")
	}

	cfg := stripenav.Config{
		StripeWebhookSecret: mustEnv("STRIPE_WEBHOOK_SECRET"),
		NAV: nav.Config{
			BaseURL:     getenv("NAV_BASE_URL", nav.TestBaseURL),
			Login:       mustEnv("NAV_LOGIN"),
			Password:    mustEnv("NAV_PASSWORD"),
			TaxNumber:   mustEnv("NAV_TAX_NUMBER"),
			SignKey:     mustEnv("NAV_SIGN_KEY"),
			ExchangeKey: mustEnv("NAV_EXCHANGE_KEY"),
			Debug:       os.Getenv("NAV_DEBUG") == "true",
			Software: nav.Software{
				ID:             getenv("NAV_SOFTWARE_ID", "HU00000000GOSTRPNV"),
				Name:           getenv("NAV_SOFTWARE_NAME", "gostripenav"),
				Operation:      getenv("NAV_SOFTWARE_OPERATION", "LOCAL_SOFTWARE"),
				MainVersion:    getenv("NAV_SOFTWARE_VERSION", "0.1.0"),
				DevName:        getenv("NAV_DEV_NAME", "gostripenav"),
				DevContact:     getenv("NAV_DEV_CONTACT", "ops@example.com"),
				DevCountryCode: getenv("NAV_DEV_COUNTRY", "HU"),
			},
		},
		Supplier: mapping.Supplier{
			TaxNumber: mustEnv("SUPPLIER_TAX_NUMBER"),
			Name:      mustEnv("SUPPLIER_NAME"),
			Address: mapping.Address{
				CountryCode:      getenv("SUPPLIER_COUNTRY", "HU"),
				PostalCode:       mustEnv("SUPPLIER_POSTAL_CODE"),
				City:             mustEnv("SUPPLIER_CITY"),
				AdditionalDetail: getenv("SUPPLIER_ADDRESS", ""),
			},
		},
		Store:                store,
		ExchangeRateProvider: devRateProvider,
		WorkerTickInterval:   parseDurationOr("WORKER_TICK_INTERVAL", 0),
	}

	if cfg.WorkerTickInterval > 0 {
		logger.Info("worker tick interval overridden", "interval", cfg.WorkerTickInterval)
	}

	h, err := stripenav.Handler(cfg)
	if err != nil {
		logger.Error("stripenav.Handler", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/webhooks/stripe", h)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	addr := getenv("LISTEN_ADDR", ":8080")
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("server listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		logger.Info("shutdown signal received", "signal", sig.String())
	case err := <-errCh:
		logger.Error("listen", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("server shutdown", "err", err)
	}
	if err := h.Shutdown(ctx); err != nil {
		logger.Error("handler shutdown", "err", err)
	}
	logger.Info("shutdown complete")
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("missing required env var", "key", key)
		os.Exit(1)
	}
	return v
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// parseDurationOr reads a duration env var. Empty / unset → fallback.
// Bad format → fallback + a warning, so a typo never crashes startup.
func parseDurationOr(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("ignoring invalid duration env var", "key", key, "value", v, "err", err)
		return fallback
	}
	return d
}

// runHealthcheck implements the `--healthcheck` self-check used by
// Docker HEALTHCHECK. Resolves LISTEN_ADDR the same way the server
// does, calls GET /healthz, and returns 0 on success / 1 on failure.
func runHealthcheck() int {
	addr := getenv("LISTEN_ADDR", ":8080")
	host := "localhost"
	if strings.HasPrefix(addr, ":") {
		// already host-less, prepend localhost
		addr = host + addr
	}
	url := "http://" + addr + "/healthz"

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "healthcheck: status %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

// devRateProvider is a dev-time stub: fixed approximate rates so that
// invoices triggered via `stripe trigger` succeed mapping end-to-end.
// Replace with a real rate source (MNB, ECB, your billing system) in
// production.
func devRateProvider(_ context.Context, currency string, _ time.Time) (string, error) {
	rates := map[string]string{
		"eur": "395.00",
		"usd": "362.00",
		"gbp": "456.00",
		"chf": "401.00",
	}
	if r, ok := rates[strings.ToLower(currency)]; ok {
		return r, nil
	}
	return "", fmt.Errorf("no dev exchange rate for %s", currency)
}
