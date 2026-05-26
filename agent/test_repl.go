package main
import (
"github.com/agentgo/internal/repl"
)
func main() {
lr := repl.New(func(in string) []string { return nil })
lr.ReadLine()
}
