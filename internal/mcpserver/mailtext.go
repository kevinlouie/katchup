package mcpserver

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/encoding/htmlindex"
)

// maxPartBytes caps how much of any single MIME part is decoded, so a huge
// attachment can't balloon memory just to report its size.
const maxPartBytes = 32 << 20

// maxMIMEDepth bounds multipart nesting; real mail rarely exceeds 4 levels.
const maxMIMEDepth = 10

// Attachment describes a non-body MIME part. Content is never returned.
type Attachment struct {
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
}

// parsedMail is a decoded .eml reduced to what a model can use: display headers,
// one text body, and an attachment manifest.
type parsedMail struct {
	From, To, Cc, Subject, Date, MessageID string
	Body                                   string
	// BodyFormat is "text/plain", "text/html" (converted to text), or "" when the
	// message has no text part.
	BodyFormat  string
	Attachments []Attachment
}

// crlf normalizes line endings to \n; CRLF only costs tokens.
var crlf = strings.NewReplacer("\r\n", "\n", "\r", "\n")

// wordDecoder decodes RFC 2047 encoded-words in any charset x/text knows
// (e.g. EUC-KR / ISO-2022-JP subjects), not just UTF-8 / Latin-1.
var wordDecoder = &mime.WordDecoder{CharsetReader: charsetReader}

func charsetReader(charset string, r io.Reader) (io.Reader, error) {
	enc, err := htmlindex.Get(charset)
	if err != nil {
		return nil, err
	}
	return enc.NewDecoder().Reader(r), nil
}

// parseMail decodes a raw RFC 5322 message. It prefers the first text/plain
// part for the body and falls back to the first text/html part converted to text.
func parseMail(raw []byte) (parsedMail, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return parsedMail{}, fmt.Errorf("parse message: %w", err)
	}

	out := parsedMail{
		From:      decodeAddrs(msg.Header.Get("From")),
		To:        decodeAddrs(msg.Header.Get("To")),
		Cc:        decodeAddrs(msg.Header.Get("Cc")),
		Subject:   decodeHeader(msg.Header.Get("Subject")),
		Date:      msg.Header.Get("Date"),
		MessageID: strings.Trim(msg.Header.Get("Message-ID"), "<> "),
	}
	if t, err := msg.Header.Date(); err == nil {
		out.Date = t.Format("2006-01-02T15:04:05-07:00")
	}

	w := walker{fallbackIdx: -1}
	w.walk(textproto.MIMEHeader(msg.Header), msg.Body, 0)
	// No filename-free text part: use the first inline text part that had a
	// filename (Outlook's ATT00001.htm) instead of reporting it as an attachment.
	if w.plain == "" && w.html == "" && w.fallbackIdx >= 0 {
		if w.attachments[w.fallbackIdx].ContentType == "text/html" {
			w.html = w.fallbackText
		} else {
			w.plain = w.fallbackText
		}
		w.attachments = append(w.attachments[:w.fallbackIdx], w.attachments[w.fallbackIdx+1:]...)
	}
	out.Attachments = w.attachments
	switch {
	case w.plain != "":
		out.Body, out.BodyFormat = strings.TrimSpace(crlf.Replace(w.plain)), "text/plain"
	case w.html != "":
		out.Body, out.BodyFormat = htmlToText(crlf.Replace(w.html)), "text/html"
	}
	return out, nil
}

type walker struct {
	plain, html string
	attachments []Attachment
	// fallbackText is the decoded first inline text part carrying a filename;
	// fallbackIdx is its entry in attachments, or -1.
	fallbackText string
	fallbackIdx  int
}

func (w *walker) walk(h textproto.MIMEHeader, body io.Reader, depth int) {
	// ParseMediaType returns the type alongside ErrInvalidMediaParameter for a
	// malformed parameter (e.g. an unquoted `name=my logo.png`), so only a
	// missing or unparseable type defaults to text/plain.
	mediaType, params, _ := mime.ParseMediaType(h.Get("Content-Type"))
	if mediaType == "" {
		mediaType = "text/plain"
	}

	if strings.HasPrefix(mediaType, "multipart/") && depth < maxMIMEDepth {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextRawPart()
			if err != nil {
				return
			}
			w.walk(part.Header, part, depth+1)
		}
	}

	cte := h.Get("Content-Transfer-Encoding")
	disposition, dparams, _ := mime.ParseMediaType(h.Get("Content-Disposition"))
	filename := decodeHeader(dparams["filename"])
	if filename == "" {
		filename = decodeHeader(params["name"])
	}
	isText := mediaType == "text/plain" || mediaType == "text/html"
	inlineText := isText && disposition != "attachment"

	// The first filename-free inline text part of each type is the body.
	if inlineText && filename == "" {
		switch {
		case mediaType == "text/plain" && w.plain == "":
			w.plain = decodeCharset(params["charset"], decodePart(cte, body))
			return
		case mediaType == "text/html" && w.html == "":
			w.html = decodeCharset(params["charset"], decodePart(cte, body))
			return
		}
	}

	att := Attachment{Filename: filename, ContentType: mediaType}
	if inlineText && filename != "" && w.fallbackIdx < 0 {
		data := decodePart(cte, body)
		w.fallbackText, w.fallbackIdx = decodeCharset(params["charset"], data), len(w.attachments)
		att.Size = len(data)
	} else {
		att.Size = partSize(cte, body)
	}
	w.attachments = append(w.attachments, att)
}

// decodePart reads a part body (up to maxPartBytes decoded) and undoes its
// Content-Transfer-Encoding.
func decodePart(cte string, r io.Reader) []byte {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		// Base64 plus line breaks is ~1.37x the decoded size.
		raw, _ := io.ReadAll(io.LimitReader(r, maxPartBytes*3/2))
		return decodeBase64Lenient(raw)
	case "quoted-printable":
		data, _ := io.ReadAll(io.LimitReader(quotedprintable.NewReader(r), maxPartBytes))
		return data
	default:
		data, _ := io.ReadAll(io.LimitReader(r, maxPartBytes))
		return data
	}
}

// partSize reports a part's decoded size without buffering it (attachment
// content is never returned). Base64 size is derived from the alphabet bytes.
func partSize(cte string, r io.Reader) int {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		var sextets int
		buf := make([]byte, 32<<10)
		for {
			n, err := r.Read(buf)
			for _, c := range buf[:n] {
				if isBase64Alphabet(c) {
					sextets++
				}
			}
			if err != nil {
				return sextets * 3 / 4
			}
		}
	case "quoted-printable":
		n, _ := io.Copy(io.Discard, quotedprintable.NewReader(r))
		return int(n)
	default:
		n, _ := io.Copy(io.Discard, r)
		return int(n)
	}
}

// decodeBase64Lenient decodes base64 that may be unpadded, split into
// separately padded chunks, or carry stray bytes (line breaks, trailing junk):
// it decodes each '='-terminated run of alphabet bytes on its own and
// concatenates the results, rather than stopping at the first irregularity.
func decodeBase64Lenient(raw []byte) []byte {
	var out []byte
	for _, chunk := range bytes.FieldsFunc(raw, func(r rune) bool { return r == '=' }) {
		clean := make([]byte, 0, len(chunk))
		for _, c := range chunk {
			if isBase64Alphabet(c) {
				clean = append(clean, c)
			}
		}
		// A lone trailing sextet can't encode a byte.
		if len(clean)%4 == 1 {
			clean = clean[:len(clean)-1]
		}
		dec := make([]byte, base64.RawStdEncoding.DecodedLen(len(clean)))
		n, _ := base64.RawStdEncoding.Decode(dec, clean)
		out = append(out, dec[:n]...)
	}
	return out
}

func isBase64Alphabet(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '+' || c == '/'
}

// decodeCharset converts data to UTF-8. Unknown charsets are passed through,
// with invalid UTF-8 sequences replaced so the result is always valid JSON text.
func decodeCharset(charset string, data []byte) string {
	if charset != "" && !strings.EqualFold(charset, "utf-8") && !strings.EqualFold(charset, "us-ascii") {
		if enc, err := htmlindex.Get(charset); err == nil {
			if dec, err := enc.NewDecoder().Bytes(data); err == nil {
				data = dec
			}
		}
	}
	if utf8.Valid(data) {
		return string(data)
	}
	return strings.ToValidUTF8(string(data), "�")
}

func decodeHeader(s string) string {
	if dec, err := wordDecoder.DecodeHeader(s); err == nil {
		return dec
	}
	return s
}

// decodeAddrs renders an address-list header as `Name <addr>, ...`, keeping
// display names (the index only stores bare addresses).
func decodeAddrs(header string) string {
	if header == "" {
		return ""
	}
	parser := mail.AddressParser{WordDecoder: wordDecoder}
	addrs, err := parser.ParseList(header)
	if err != nil {
		return decodeHeader(header)
	}
	out := make([]string, len(addrs))
	for i, a := range addrs {
		if a.Name != "" {
			out[i] = a.Name + " <" + a.Address + ">"
		} else {
			out[i] = a.Address
		}
	}
	return strings.Join(out, ", ")
}

var (
	reDropBlocks = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<script\b.*?</script\s*>`),
		regexp.MustCompile(`(?is)<style\b.*?</style\s*>`),
		regexp.MustCompile(`(?is)<head\b.*?</head\s*>`),
		regexp.MustCompile(`(?s)<!--.*?-->`),
	}
	reBreak = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|tr|li|h[1-6]|table|blockquote|ul|ol)\s*>`)
	reCell  = regexp.MustCompile(`(?i)</t[dh]\s*>`)
	// A tag runs to the first '>' outside a quoted attribute value.
	reTag       = regexp.MustCompile(`<(?:[^>"']|"[^"]*"|'[^']*')*>`)
	reHSpace    = regexp.MustCompile(`[ \t\x{00A0}]+`)
	reBlankRuns = regexp.MustCompile(`\n{3,}`)
)

// htmlToText is a deliberately small HTML flattener: drop script/style/head,
// turn block ends into newlines, strip tags, unescape entities, squeeze
// whitespace. Good enough for reading mail, not a renderer.
func htmlToText(s string) string {
	for _, re := range reDropBlocks {
		s = re.ReplaceAllString(s, "")
	}
	s = reBreak.ReplaceAllString(s, "\n")
	s = reCell.ReplaceAllString(s, " ")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)

	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(reHSpace.ReplaceAllString(l, " "))
	}
	return strings.TrimSpace(reBlankRuns.ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}
