package main

import (
	"log"
	"os"

	"codex-agent/internal/server"
)

func main() {
	if err := server.Run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
