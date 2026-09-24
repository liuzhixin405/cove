package tool

import (
	"bytes"
	"unicode/utf8"
)

type textKind int

const (
	textUTF8 textKind = iota
	textNotUTF8
	textUTF16
	textBinary
)

// sniffText classifies a file from its first bytes. A NUL byte means binary,
// except in UTF-16 text, which PowerShell 5 writes with a byte-order mark.
func sniffText(sample []byte) textKind {
	if bytes.HasPrefix(sample, []byte{0xFF, 0xFE}) || bytes.HasPrefix(sample, []byte{0xFE, 0xFF}) {
		return textUTF16
	}
	if bytes.IndexByte(sample, 0) >= 0 {
		return textBinary
	}
	// The sample may end inside a multi-byte rune; judge only complete ones.
	for len(sample) > 0 {
		r, size := utf8.DecodeRune(sample)
		if r == utf8.RuneError && size <= 1 {
			if !utf8.FullRune(sample) {
				break
			}
			return textNotUTF8
		}
		sample = sample[size:]
	}
	return textUTF8
}
