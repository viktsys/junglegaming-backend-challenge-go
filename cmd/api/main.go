// Command api starts the wager processing service: HTTP API, SQS consumer,
// reference resolver and outbox publisher, composed with Uber Fx.
package main

import (
	"os"

	"github.com/junglegaming/backend-challenge-go/internal/bootstrap"
)

func main() {
	app := bootstrap.NewApp()
	app.Run()
	// Run returns after a signal-triggered stop; surface startup errors.
	if err := app.Err(); err != nil {
		os.Exit(1)
	}
}
