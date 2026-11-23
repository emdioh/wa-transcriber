package main

// lines turns the console byte stream into lines. The board's tty writes
// "\r\r\n" for every line break (the shell writes \r\n on a raw tty and the
// gadget duplicates the \r); a bare \r is what getty and sh echo for the
// Enter we send; and the kernel can drop an unrelated line into the middle of
// a half-received one. The rule that survives all three: any run of \r and \n
// ends one line and starts the next, and the empty pieces that produces are
// dropped. A prompt printed without a newline never completes a line: it stays
// in buf and the session sees it as a partial line, which is how it tells the
// login prompt (which has no newline after it) from output still arriving.
type lines struct {
	buf []byte
}

// feed appends data and returns every line that was completed by it, without
// their terminators. A long line with no terminator at all is returned once
// it passes maxLine so a board that spams without newlines cannot grow the
// buffer without bound.
const maxLine = 4096

func (l *lines) feed(data []byte) []string {
	var out []string
	for _, b := range data {
		switch b {
		case '\r', '\n':
			if len(l.buf) > 0 {
				out = append(out, string(l.buf))
				l.buf = l.buf[:0]
			}
			continue
		}
		if b == 0 {
			// A CDC-ACM frame that was cut short can carry NULs; they are
			// not text and would make every comparison fail.
			continue
		}
		l.buf = append(l.buf, b)
		if len(l.buf) >= maxLine {
			out = append(out, string(l.buf))
			l.buf = l.buf[:0]
		}
	}
	return out
}

// flush returns the unterminated remainder, if any. It is used when the
// session ends so a partial line is not lost.
func (l *lines) flush() (string, bool) {
	if len(l.buf) == 0 {
		return "", false
	}
	s := string(l.buf)
	l.buf = l.buf[:0]
	return s, true
}
