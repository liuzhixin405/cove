package tool

import (
	"bytes"
	"fmt"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// legacyText is a file decoded from GBK or GB18030, with the encoding to write
// it back in. Chinese Windows tools (Notepad before 1903, Visual Studio with
// the system code page, many build scripts) still write GBK, and read used to
// show such files as mojibake while edit refused them outright.
type legacyText struct {
	text string
	enc  encoding.Encoding
	name string
}

// gbkMinCommonRatio is the share of double-byte characters that must fall in
// the GB2312 rows (see isGB2312Pair) before bytes are taken for GBK.
//
// Structural validity alone is a poor test: GBK accepts any lead byte
// 0x81-0xFE followed by 0x40-0x7E, so Latin-1 "caféine" (é = 0xE9, then 'i')
// is valid GBK and decoded to a rare hanzi, and the model would go on to write
// GBK into a Latin-1 file. Real Simplified Chinese text is almost all GB2312,
// where both bytes are >= 0xA1; Latin-1 accents are mostly followed by ASCII,
// and Big5 or Shift-JIS put about half their trail bytes below 0x80. The
// remaining false positive is Latin-1 with two accented letters in a row
// ("Größe"), which decodes but round-trips byte for byte, so untouched text is
// never damaged.
const gbkMinCommonRatio = 0.8

// decodeGBK decodes data as GBK, or as GB18030 when it has four-byte
// sequences, if it plausibly is that. The decoded text must encode back to
// exactly data, so an edit that leaves a region alone leaves its bytes alone.
func decodeGBK(data []byte) (legacyText, bool) {
	common, other, fourByte := 0, 0, false
	for i := 0; i < len(data); {
		c0 := data[i]
		switch {
		case c0 < 0x80:
			i++
			continue
		case c0 == 0x80: // the euro sign in code page 936
			other++
			i++
			continue
		case c0 == 0xFF || i+1 >= len(data):
			return legacyText{}, false
		}
		c1 := data[i+1]
		switch {
		case 0x30 <= c1 && c1 <= 0x39:
			if i+3 >= len(data) || data[i+2] < 0x81 || data[i+2] == 0xFF || data[i+3] < 0x30 || data[i+3] > 0x39 {
				return legacyText{}, false
			}
			fourByte = true
			i += 4
		case 0x40 <= c1 && c1 <= 0xFE && c1 != 0x7F:
			if isGB2312Pair(c0, c1) {
				common++
			} else {
				other++
			}
			i += 2
		default:
			return legacyText{}, false
		}
	}
	if common == 0 || float64(common) < gbkMinCommonRatio*float64(common+other) {
		return legacyText{}, false
	}

	lt := legacyText{enc: simplifiedchinese.GBK, name: "GBK"}
	if fourByte {
		lt.enc, lt.name = simplifiedchinese.GB18030, "GB18030"
	}
	decoded, err := lt.enc.NewDecoder().Bytes(data)
	if err != nil {
		return legacyText{}, false
	}
	// Unassigned codes decode to U+FFFD, and a few codes share a character
	// (0x80 and 0xA2E3 are both the euro sign); either way the file would not
	// survive a rewrite, so it is not treated as GBK.
	if back, err := lt.enc.NewEncoder().Bytes(decoded); err != nil || !bytes.Equal(back, data) {
		return legacyText{}, false
	}
	lt.text = string(decoded)
	return lt, true
}

// isGB2312Pair reports whether a double-byte GBK code is in GB2312: the symbol
// rows 0xA1-0xA9 (punctuation, full-width forms, box drawing, pinyin) and the
// hanzi rows 0xB0-0xF7, each with a trail byte of 0xA1-0xFE.
func isGB2312Pair(c0, c1 byte) bool {
	return c1 >= 0xA1 && c1 <= 0xFE && ((c0 >= 0xA1 && c0 <= 0xA9) || (c0 >= 0xB0 && c0 <= 0xF7))
}

// encode converts text back to the file's encoding. It fails, naming the first
// character the encoding lacks, rather than writing a replacement for it.
func (lt legacyText) encode(text string) ([]byte, error) {
	out, err := lt.enc.NewEncoder().Bytes([]byte(text))
	if err == nil {
		return out, nil
	}
	for _, r := range text {
		if _, err := lt.enc.NewEncoder().String(string(r)); err != nil {
			return nil, fmt.Errorf("%s cannot represent %q (U+%04X)", lt.name, r, r)
		}
	}
	return nil, err
}

// encodeFailure is the tool error for an encode failure; field names the
// input that carried the character (newString, content).
func (lt legacyText) encodeFailure(path, field string, err error) Result {
	return Result{Data: fmt.Sprintf("Error: %s is %s-encoded, and %v in %s. The file was left unchanged; use characters %s has, or convert the file to UTF-8 first (e.g. iconv -f %s -t UTF-8).",
		path, lt.name, err, field, lt.name, lt.name), IsError: true}
}
