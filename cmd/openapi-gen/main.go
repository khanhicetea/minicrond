package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/khanhicetea/minicrond/internal/api"
)

func main() {
	if err := generate(os.Stdout); err != nil {
		slog.Error("OpenAPI generation failed", "error", err)
		os.Exit(1)
	}
}

func generate(out io.Writer) error {
	if _, err := fmt.Fprintf(out, "%s\n", api.OpenAPIContract("0.2.0")); err != nil {
		return fmt.Errorf("write OpenAPI contract: %w", err)
	}
	return nil
}
