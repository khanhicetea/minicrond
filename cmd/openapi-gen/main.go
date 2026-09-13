package main

import (
	"os"

	"github.com/khanhicetea/minicrond/api"
)

func main() {
	if _, err := os.Stdout.Write(append(api.OpenAPIContract("0.1.0"), '\n')); err != nil {
		panic(err)
	}
}
