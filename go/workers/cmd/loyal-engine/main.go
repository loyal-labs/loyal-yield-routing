package main

import (
	"errors"
	"fmt"
	"os"
)

// Composition is installed after each family's actual runtime is integrated.
// Reject startup during construction instead of pretending to process work.
func run() error {
	return errors.New("engine runtime integration incomplete; use loyal-evidence for saved replay")
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
