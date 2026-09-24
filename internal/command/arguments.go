package command

import "strings"

// argumentsPlaceholder is where a Claude-format command file wants the
// arguments typed after the command.
const argumentsPlaceholder = "$ARGUMENTS"

// ExpandArguments fills a custom or plugin slash command's prompt body with
// the arguments typed after it. Every $ARGUMENTS is replaced; a body without
// the placeholder gets the arguments appended as a separate paragraph.
//
// The front ends used to append unconditionally, so a Claude-format command
// ("Fix issue $ARGUMENTS ...") reached the model with the literal placeholder
// and the real arguments dangling after it.
func ExpandArguments(body, args string) string {
	args = strings.TrimSpace(args)
	if strings.Contains(body, argumentsPlaceholder) {
		return strings.ReplaceAll(body, argumentsPlaceholder, args)
	}
	if args == "" {
		return body
	}
	return body + "\n\n" + args
}
