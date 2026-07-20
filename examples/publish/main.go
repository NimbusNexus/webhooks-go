// Command publish is a runnable example: publish one event with the SDK.
//
//	WEBHOOKD_URL=https://api.webhooks.example WEBHOOKD_API_KEY=whsk_... go run ./examples/publish
//
// This mirrors examples/python/publish.py and examples/typescript/publish.ts.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	webhookd "github.com/NimbusNexus/webhookd-go"
)

func main() {
	baseURL := os.Getenv("WEBHOOKD_URL")
	apiKey := os.Getenv("WEBHOOKD_API_KEY")
	if baseURL == "" || apiKey == "" {
		log.Fatal("set WEBHOOKD_URL and WEBHOOKD_API_KEY")
	}

	wh := webhookd.New(baseURL, apiKey)

	event, err := wh.Publish(
		context.Background(),
		"order.created",
		map[string]any{"order_id": "ord_123", "total": 4200},
		&webhookd.PublishOptions{IdempotencyKey: "order-123-created"}, // makes the publish safe to retry
	)
	if err != nil {
		var apiErr *webhookd.APIError
		if errors.As(err, &apiErr) {
			log.Fatalf("publish failed: [%d %s] %s", apiErr.StatusCode, apiErr.Code, apiErr.Message)
		}
		log.Fatalf("publish failed: %v", err)
	}

	fmt.Printf("published %s -> %d deliveries\n", event.EventUID, event.DeliveriesCreated)
}
