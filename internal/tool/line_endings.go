package tool

import "strings"

// utf8BOM is the byte-order mark some Windows tools write at the start of
// UTF-8 files (Visual Studio projects, .config files, PowerShell scripts).
const utf8BOM = "\ufeff"

// usesCRLF reports whether most line breaks in content are CRLF.
func usesCRLF(content string) bool {
	crlf := strings.Count(content, "\r\n")
	return crlf > 0 && crlf >= strings.Count(content, "\n")-crlf
}

// toCRLF converts every line break in text to CRLF.
func toCRLF(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")
}

// matchExistingFile adapts text the model wrote to the conventions of the file
// it replaces. The read tool shows lines without their \r, so the model always
// writes LF; on a CRLF file that turned every line into a diff, and a missing
// BOM changes how some Windows tools decode the file.
func matchExistingFile(text, existing string) string {
	if usesCRLF(existing) {
		text = toCRLF(text)
	}
	if strings.HasPrefix(existing, utf8BOM) && !strings.HasPrefix(text, utf8BOM) {
		text = utf8BOM + text
	}
	return text
}
