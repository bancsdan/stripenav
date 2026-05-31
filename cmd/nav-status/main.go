// nav-status is an operator's hand tool: it calls NAV's
// /queryTransactionStatus for a given transactionId and prints a
// readable summary plus (optionally) the raw XML response.
//
// Usage:
//
//	go run ./cmd/nav-status <transactionId>
//
// Reads the same NAV_* env vars as cmd/gostripenav, so `task nav:status
// TX=...` picks up the local .env automatically.
package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"log"
	"os"

	"github.com/bancsdan/go-stripenav/nav"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: nav-status <transactionId>")
	}
	txID := os.Args[1]

	cfg := nav.Config{
		BaseURL:     getenv("NAV_BASE_URL", nav.TestBaseURL),
		Login:       must("NAV_LOGIN"),
		Password:    must("NAV_PASSWORD"),
		TaxNumber:   must("NAV_TAX_NUMBER"),
		SignKey:     must("NAV_SIGN_KEY"),
		ExchangeKey: must("NAV_EXCHANGE_KEY"),
		Software: nav.Software{
			ID:             getenv("NAV_SOFTWARE_ID", "HU00000000GOSTRPNV"),
			Name:           "gostripenav status query",
			Operation:      "LOCAL_SOFTWARE",
			MainVersion:    "0.1.0",
			DevName:        getenv("NAV_DEV_NAME", "gostripenav"),
			DevContact:     getenv("NAV_DEV_CONTACT", "ops@example.com"),
			DevCountryCode: "HU",
		},
		Debug: os.Getenv("NAV_DEBUG") == "true",
	}

	client, err := nav.NewClient(cfg)
	if err != nil {
		log.Fatalf("nav.NewClient: %v", err)
	}

	resp, err := client.QueryTransactionStatus(context.Background(), txID, true)
	if err != nil {
		log.Fatalf("queryTransactionStatus: %v", err)
	}

	fmt.Printf("transactionId   : %s\n", txID)
	fmt.Printf("funcCode        : %s\n", resp.Result.FuncCode)
	if resp.Result.ErrorCode != "" {
		fmt.Printf("errorCode       : %s\n", resp.Result.ErrorCode)
		fmt.Printf("message         : %s\n", resp.Result.Message)
	}
	fmt.Printf("originalVersion : %s\n", resp.ProcessingResults.OriginalRequestVersion)
	if ad := resp.ProcessingResults.AnnulmentData; ad != nil {
		fmt.Printf("\nannulmentVerification:\n")
		fmt.Printf("  status        : %s\n", ad.AnnulmentVerificationStatus)
		if ad.AnnulmentDecisionDate != "" {
			fmt.Printf("  decision date : %s\n", ad.AnnulmentDecisionDate)
			fmt.Printf("  decision user : %s\n", ad.AnnulmentDecisionUser)
		}
	}
	for _, pr := range resp.ProcessingResults.ProcessingResult {
		fmt.Printf("\nprocessingResult[index=%d]:\n", pr.Index)
		fmt.Printf("  invoiceStatus               : %s\n", pr.InvoiceStatus)
		if len(pr.TechnicalValidationMessages) > 0 {
			fmt.Printf("  technicalValidationMessages :\n")
			for _, m := range pr.TechnicalValidationMessages {
				fmt.Printf("    - %-8s %s: %s\n", m.ValidationResultCode, m.ValidationErrorCode, m.Message)
			}
		}
		if len(pr.BusinessValidationMessages) > 0 {
			fmt.Printf("  businessValidationMessages  :\n")
			for _, m := range pr.BusinessValidationMessages {
				fmt.Printf("    - %-8s %s: %s\n", m.ValidationResultCode, m.ValidationErrorCode, m.Message)
				if m.Pointer != nil && m.Pointer.Tag != "" {
					fmt.Printf("        @ tag=%s value=%q line=%d\n", m.Pointer.Tag, m.Pointer.Value, m.Pointer.Line)
				}
			}
		}
	}

	if os.Getenv("NAV_DUMP_XML") == "true" {
		out, _ := xml.MarshalIndent(resp, "", "  ")
		fmt.Printf("\n--- raw response ---\n%s\n", out)
	}
}

func must(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("missing required env var %s", key)
	}
	return v
}

func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
