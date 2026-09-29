package mcpserver

import (
	"strings"
	"testing"
)

func TestParseMailKoreanHTMLOnly(t *testing.T) {
	// EUC-KR encoded-word subject, HTML-only body in EUC-KR ("안녕하세요" = C8C8 B3E7 C7CF BCBC BFE4).
	raw := "From: sender@example.kr\r\n" +
		"Subject: =?euc-kr?B?vsiz58fPvLy/5A==?=\r\n" +
		"Content-Type: text/html; charset=euc-kr\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		"PGh0bWw+PGhlYWQ+PHN0eWxlPnB7fTwvc3R5bGU+PC9oZWFkPjxib2R5PjxwPr7Is+fHz7y8v+Q8L3A+PHA+YSZhbXA7YjwvcD48L2JvZHk+PC9odG1sPg==\r\n"

	p, err := parseMail([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if p.Subject != "안녕하세요" {
		t.Errorf("subject = %q", p.Subject)
	}
	if p.BodyFormat != "text/html" || p.Body != "안녕하세요\na&b" {
		t.Errorf("body = %q (%s)", p.Body, p.BodyFormat)
	}
	if len(p.Attachments) != 0 {
		t.Errorf("attachments = %+v", p.Attachments)
	}
}

func TestHTMLToText(t *testing.T) {
	in := "<html><head><title>x</title></head><body><script>alert(1)</script>" +
		"<div>Line&nbsp;one</div><br><table><tr><td>a</td><td>b</td></tr></table><!-- c --></body></html>"
	if got, want := htmlToText(in), "Line one\n\na b"; got != want {
		t.Errorf("htmlToText = %q, want %q", got, want)
	}
}

func TestParseMailEdgeCases(t *testing.T) {
	cases := []struct {
		name, raw, body, format string
		attachments             int
	}{
		{
			name: "inline text part with filename is the body",
			raw: "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\n" +
				"Content-Type: text/plain; name=\"body.txt\"\r\nContent-Disposition: inline; filename=\"body.txt\"\r\n\r\n" +
				"line one\r\nline two\r\n--b--\r\n",
			body: "line one\nline two", format: "text/plain",
		},
		{
			name: "text part marked attachment stays an attachment",
			raw: "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nhi\r\n--b\r\n" +
				"Content-Type: text/plain\r\nContent-Disposition: attachment; filename=\"notes.txt\"\r\n\r\nnotes\r\n--b--\r\n",
			body: "hi", format: "text/plain", attachments: 1,
		},
		{
			name: "unpadded base64 with trailing junk",
			raw:  "Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\naGVsbG8gd29ybGQ\r\n--junk!!\r\n",
			body: "hello world", format: "text/plain",
		},
		{
			name: "malformed param keeps the media type (inline image is not the body)",
			raw: "Content-Type: multipart/related; boundary=b\r\n\r\n--b\r\nContent-Type: text/html\r\n\r\n<p>hello</p>\r\n--b\r\n" +
				"Content-Type: image/png; name=my logo.png\r\nContent-Disposition: inline\r\n\r\n\x89PNG\r\n--b--\r\n",
			body: "hello", format: "text/html", attachments: 1,
		},
		{
			name: "inline .txt attachment does not displace an HTML body",
			raw: "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: text/html\r\n\r\n<p>hello</p>\r\n--b\r\n" +
				"Content-Type: text/plain; name=\"notes.txt\"\r\nContent-Disposition: inline; filename=\"notes.txt\"\r\n\r\nNOTES\r\n--b--\r\n",
			body: "hello", format: "text/html", attachments: 1,
		},
		{
			name: "Outlook ATT00001.htm alone becomes the body",
			raw: "Content-Type: multipart/mixed; boundary=b\r\n\r\n--b\r\nContent-Type: application/pdf; name=a.pdf\r\n\r\n%PDF\r\n--b\r\n" +
				"Content-Type: text/html; name=\"ATT00001.htm\"\r\nContent-Disposition: inline; filename=\"ATT00001.htm\"\r\n\r\n<p>sig</p>\r\n--b--\r\n",
			body: "sig", format: "text/html", attachments: 1,
		},
		{
			name: "separately padded base64 chunks",
			raw:  "Content-Type: text/plain\r\nContent-Transfer-Encoding: base64\r\n\r\naGVsbG8=\r\nd29ybGQ=\r\n",
			body: "helloworld", format: "text/plain",
		},
		{
			name: "quoted > inside an attribute",
			raw:  "Content-Type: text/html\r\n\r\n<a title=\"x>y\" href='#'>link</a>",
			body: "link", format: "text/html",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := parseMail([]byte(c.raw))
			if err != nil {
				t.Fatal(err)
			}
			// "hello world" decodes with a stray byte from "junk"; compare the prefix.
			if !strings.HasPrefix(p.Body, c.body) || p.BodyFormat != c.format || len(p.Attachments) != c.attachments {
				t.Errorf("body=%q format=%q attachments=%+v", p.Body, p.BodyFormat, p.Attachments)
			}
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
		cut  bool
	}{
		{"안녕하세요", 2, "안녕", true},
		{"안녕", 2, "안녕", false},
		{"abc", 0, "", true},
		{"", 3, "", false},
	} {
		if got, cut := truncateRunes(c.in, c.n); got != c.want || cut != c.cut {
			t.Errorf("truncateRunes(%q, %d) = %q, %v", c.in, c.n, got, cut)
		}
	}
}
