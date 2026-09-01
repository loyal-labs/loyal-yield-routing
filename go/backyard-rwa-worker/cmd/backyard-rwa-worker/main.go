package main

import (
	"context"
	"log"
	"os"

	"github.com/loyal-labs/loyal-yield-routing/go/backyard-rwa-worker/internal/backyardrwa"
)

func main() {
	if err := backyardrwa.Run(context.Background(), os.Stdout); err != nil {
		log.Fatal(err)
	}
}
