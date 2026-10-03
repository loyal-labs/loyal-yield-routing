package main

import (
	"errors"
	"fmt"
	"os"
)

func run() error {
	return errors.New("observer runtime integration incomplete; live processing unavailable")
}
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
