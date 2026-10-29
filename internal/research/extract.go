package research

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/url"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

const ExtractorVersion = "openfactory-text-v1"

type Extracted struct {
	Text, MediaType, CanonicalURL string
	Links                         []string
}

// Extract never silently truncates. The text blob is a separate representation
// from the complete HTTP body and has its own hash and extractor identity.
func Extract(body []byte, contentType, finalURL string, max int64) (Extracted, error) {
	if max <= 0 || max > MaxTextBytes {
		max = MaxTextBytes
	}
	media, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return Extracted{}, rejected("source_rejected")
	}
	if charset := strings.ToLower(params["charset"]); charset != "" && charset != "utf-8" && charset != "utf8" && charset != "us-ascii" {
		return Extracted{}, rejected("source_rejected")
	}
	if !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
		return Extracted{}, rejected("source_rejected")
	}
	result := Extracted{MediaType: media}
	switch media {
	case "application/json", "application/ld+json":
		var value any
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.UseNumber()
		if decoder.Decode(&value) != nil {
			return result, rejected("source_rejected")
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return result, rejected("source_rejected")
		}
		text, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return result, rejected("source_rejected")
		}
		result.Text = string(text) + "\n"
	case "text/plain", "text/markdown", "text/x-markdown":
		result.Text = strings.ReplaceAll(string(body), "\r\n", "\n")
	case "text/html", "application/xhtml+xml":
		base, err := url.Parse(finalURL)
		if err != nil {
			return result, rejected("source_rejected")
		}
		tokenizer := html.NewTokenizer(bytes.NewReader(body))
		var out strings.Builder
		ignored := 0
		seen := map[string]bool{}
		for {
			typ := tokenizer.Next()
			if typ == html.ErrorToken {
				if tokenizer.Err() != io.EOF {
					return result, rejected("source_rejected")
				}
				break
			}
			switch typ {
			case html.StartTagToken, html.SelfClosingTagToken:
				token := tokenizer.Token()
				name := strings.ToLower(token.Data)
				if name == "script" || name == "style" || name == "noscript" || name == "template" {
					if typ == html.StartTagToken {
						ignored++
					}
					continue
				}
				if ignored > 0 {
					continue
				}
				if name == "a" || name == "link" {
					href, rel := "", ""
					for _, a := range token.Attr {
						if a.Key == "href" {
							href = a.Val
						}
						if a.Key == "rel" {
							rel = a.Val
						}
					}
					if href != "" {
						link, err := base.Parse(href)
						if err == nil {
							link.Fragment = ""
							safe, err := PublicURL(link.String())
							if err == nil {
								value := safe.String()
								if name == "link" && strings.EqualFold(rel, "canonical") {
									result.CanonicalURL = value
								}
								if name == "a" && len(result.Links) < 100 && !seen[value] {
									result.Links = append(result.Links, value)
									seen[value] = true
								}
							}
						}
					}
				}
				if blockTag(name) {
					out.WriteByte('\n')
				}
			case html.EndTagToken:
				token := tokenizer.Token()
				name := strings.ToLower(token.Data)
				if name == "script" || name == "style" || name == "noscript" || name == "template" {
					if ignored > 0 {
						ignored--
					}
					continue
				}
				if ignored == 0 && blockTag(name) {
					out.WriteByte('\n')
				}
			case html.TextToken:
				if ignored == 0 {
					out.WriteString(strings.Join(strings.Fields(string(tokenizer.Text())), " "))
					out.WriteByte(' ')
				}
			}
			if int64(out.Len()) > max {
				return result, rejected("source_too_large")
			}
		}
		lines := strings.Split(out.String(), "\n")
		var normalized strings.Builder
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" {
				normalized.WriteString(line)
				normalized.WriteByte('\n')
			}
		}
		result.Text = normalized.String()
	default:
		return result, rejected("unsupported_media_type")
	}
	if int64(len(result.Text)) > max {
		return result, rejected("source_too_large")
	}
	if strings.TrimSpace(result.Text) == "" {
		return result, rejected("source_rejected")
	}
	return result, nil
}
func blockTag(name string) bool {
	switch name {
	case "p", "br", "div", "section", "article", "header", "footer", "nav", "main", "li", "h1", "h2", "h3", "h4", "h5", "h6", "pre", "blockquote", "tr":
		return true
	}
	return false
}

// VerifyQuote establishes occurrence only, not truth, publisher identity, or
// semantic entailment. Lines are one-based inclusive in the captured text blob.
func VerifyQuote(text, quote string, startLine, endLine int) bool {
	if quote == "" || len(quote) > 64<<10 || startLine < 0 || endLine < 0 {
		return false
	}
	if startLine == 0 && endLine == 0 {
		return strings.Contains(text, quote)
	}
	if startLine < 1 || endLine < startLine {
		return false
	}
	lines := strings.Split(text, "\n")
	if endLine > len(lines) {
		return false
	}
	return strings.Contains(strings.Join(lines[startLine-1:endLine], "\n"), quote)
}
