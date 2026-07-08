package tghtml

// Split breaks an oversized Telegram-HTML (or plain-text) message into chunks,
// each within limit UTF-16 units, and reports whether it fit in ≤maxParts of them.
//
// The only break points are newlines at tag depth 0 — where no element is open.
// A chunk boundary there leaves every tag opened within a chunk also closed within
// it, so each chunk is independently valid Telegram HTML; a long <pre>/<code>/
// <blockquote> never tears mid-element (its inner newlines sit at depth > 0). The
// separating newline is consumed at the boundary (the message split represents it).
//
// ok is false — the caller then spills or errors per the overflow policy — when a
// single indivisible atom exceeds limit (e.g. a <pre> block, or one long line with
// no depth-0 newline), or when packing needs more than maxParts chunks.
func Split(text string, limit, maxParts int) (parts []string, ok bool) {
	var cur string
	have := false
	for _, atom := range splitAtDepthZero(text) {
		cand := atom
		if have {
			cand = cur + "\n" + atom
		}
		if UTF16Len(cand) <= limit {
			cur, have = cand, true
			continue
		}
		// atom does not fit onto the current chunk — flush it and start anew.
		if have {
			parts = append(parts, cur)
		}
		if UTF16Len(atom) > limit {
			return nil, false // atom is indivisible and itself over the limit
		}
		cur, have = atom, true
	}
	if have {
		parts = append(parts, cur)
	}
	if len(parts) > maxParts {
		return nil, false
	}
	return parts, true
}

// splitAtDepthZero cuts text into the atoms between newlines that sit at tag depth
// 0. Depth is tracked with the same tagNameRe the HTML guard uses (+1 per start
// tag, −1 per end tag); the whitelist has no void tags, so every tag is paired.
// Newlines inside an open element are kept (depth > 0), so its atom stays whole.
func splitAtDepthZero(text string) []string {
	locs := tagNameRe.FindAllStringIndex(text, -1)
	var atoms []string
	depth, start, ti := 0, 0, 0
	for i := 0; i < len(text); {
		if ti < len(locs) && i == locs[ti][0] {
			if text[i+1] == '/' {
				if depth > 0 {
					depth--
				}
			} else {
				depth++
			}
			i = locs[ti][1]
			ti++
			continue
		}
		if text[i] == '\n' && depth == 0 {
			atoms = append(atoms, text[start:i])
			start = i + 1
		}
		i++
	}
	return append(atoms, text[start:])
}

// UTF16Len returns the number of UTF-16 code units in s — how Telegram counts a
// message's length. Runes above the BMP encode as a surrogate pair (two units).
func UTF16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}
