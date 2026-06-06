//go:build manualtest

package main

import (
	"github.com/liuzhixin405/cove/internal/repl"
)

func main() {
	lr := repl.New(func(in string) []string { return nil })
	lr.ReadLine()
}
