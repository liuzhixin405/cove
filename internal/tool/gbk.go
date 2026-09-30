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
	var st gbkStats
	if !st.scan(data) {
		return legacyText{}, false
	}
	lt, ok := st.encoding()
	if !ok {
		return legacyText{}, false
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

// gbkStats is the structural half of decodeGBK's test, accumulated over one
// or more pieces of a file.
type gbkStats struct {
	common, other int
	fourByte      bool
}

// scan adds data's double-byte codes to the counts, reporting false if data
// is not structurally GBK/GB18030. A piece must not end inside a sequence;
// a newline byte never occurs inside one, so pieces split at newlines are
// judged exactly as the whole file would be.
func (st *gbkStats) scan(data []byte) bool {
	for i := 0; i < len(data); {
		c0 := data[i]
		switch {
		case c0 < 0x80:
			i++
			continue
		case c0 == 0x80: // the euro sign in code page 936
			st.other++
			i++
			continue
		case c0 == 0xFF || i+1 >= len(data):
			return false
		}
		c1 := data[i+1]
		switch {
		case 0x30 <= c1 && c1 <= 0x39:
			if i+3 >= len(data) || data[i+2] < 0x81 || data[i+2] == 0xFF || data[i+3] < 0x30 || data[i+3] > 0x39 {
				return false
			}
			st.fourByte = true
			i += 4
		case 0x40 <= c1 && c1 <= 0xFE && c1 != 0x7F:
			if isGB2312Pair(c0, c1) {
				st.common++
			} else {
				st.other++
			}
			i += 2
		default:
			return false
		}
	}
	return true
}

// encoding picks GBK or GB18030 when the counts look like Chinese text.
func (st *gbkStats) encoding() (legacyText, bool) {
	if st.common == 0 || float64(st.common) < gbkMinCommonRatio*float64(st.common+st.other) {
		return legacyText{}, false
	}
	lt := legacyText{enc: simplifiedchinese.GBK, name: "GBK"}
	if st.fourByte {
		lt.enc, lt.name = simplifiedchinese.GB18030, "GB18030"
	}
	return lt, true
}

// gbkStream makes decodeGBK's decision over a file fed one line at a time, so
// read can classify a file of any size without holding it in memory. Neither
// the structure test nor the round trip crosses a newline, so the verdict is
// the one decodeGBK would give for the whole content. Which encoding applies
// is known only at the end (one four-byte sequence anywhere means GB18030),
// so each line's round trip is checked for both.
type gbkStream struct {
	stats           gbkStats
	invalid         bool
	gbkBad, gb18Bad bool
}

func (s *gbkStream) feed(line []byte) {
	if s.invalid {
		return
	}
	if !s.stats.scan(line) {
		s.invalid = true
		return
	}
	if isASCII(line) {
		return
	}
	s.gbkBad = s.gbkBad || !roundTrips(simplifiedchinese.GBK, line)
	s.gb18Bad = s.gb18Bad || !roundTrips(simplifiedchinese.GB18030, line)
}

// result is the encoding to decode the file with (without text), if any.
func (s *gbkStream) result() (legacyText, bool) {
	if s.invalid {
		return legacyText{}, false
	}
	lt, ok := s.stats.encoding()
	if !ok || (lt.name == "GBK" && s.gbkBad) || (lt.name == "GB18030" && s.gb18Bad) {
		return legacyText{}, false
	}
	return lt, true
}

func roundTrips(enc encoding.Encoding, p []byte) bool {
	decoded, err := enc.NewDecoder().Bytes(p)
	if err != nil {
		return false
	}
	back, err := enc.NewEncoder().Bytes(decoded)
	return err == nil && bytes.Equal(back, p)
}

func isASCII(p []byte) bool {
	for _, c := range p {
		if c >= 0x80 {
			return false
		}
	}
	return true
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
