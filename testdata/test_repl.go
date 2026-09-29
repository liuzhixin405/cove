//go:build manualtest

package main

import (
	"github.com/liuzhixin405/cove-agent/internal/repl"
)

func main() {
	lr := repl.New(func(in string) []string { return nil })
	lr.ReadLine()
}
