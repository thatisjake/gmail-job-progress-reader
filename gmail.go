package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
	"google.golang.org/api/gmail/v1"
)

const defaultKeywords = "interview,application,candidate,recruiting,recruiter,next step,next round,hiring,position,opportunity"

type emailMessage struct {
	ID        string
	ThreadID  string
	From      string
	Subject   string
	Received  time.Time
	Body      string
	Truncated bool
}

func buildQuery(days int, keywords string, includeJobAlerts bool) (string, error) {
	if days < 1 {
		return "", fmt.Errorf("days must be positive")
	}
	var terms []string
	for _, keyword := range strings.Split(keywords, ",") {
		keyword = strings.TrimSpace(keyword)
		if keyword == "" {
			continue
		}
		for _, character := range keyword {
			if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == ' ' || character == '-') {
				return "", fmt.Errorf("keywords must contain only letters, digits, spaces, or hyphens")
			}
		}
		terms = append(terms, `"`+keyword+`"`)
	}
	if len(terms) == 0 {
		return "", fmt.Errorf("at least one keyword is required")
	}
	query := fmt.Sprintf("newer_than:%dd {%s}", days, strings.Join(terms, " "))
	if !includeJobAlerts {
		query += " -from:jobalerts-noreply@linkedin.com -from:jobalert@lensa.com"
	}
	return query, nil
}

func findCandidates(ctx context.Context, service *gmail.Service, query string, limit int) ([]string, bool, error) {
	if limit < 1 {
		return nil, false, fmt.Errorf("limit must be positive")
	}
	var identifiers []string
	seen := make(map[string]bool)
	pageToken := ""
	seenPages := make(map[string]bool)
	for {
		response, err := service.Users.Messages.List("me").LabelIds("INBOX").Q(query).
			MaxResults(100).PageToken(pageToken).Context(ctx).Do()
		if err != nil {
			return nil, false, fmt.Errorf("Gmail search failed: %w", err)
		}
		for _, message := range response.Messages {
			if seen[message.Id] {
				continue
			}
			if len(identifiers) == limit {
				return identifiers, true, nil
			}
			seen[message.Id] = true
			identifiers = append(identifiers, message.Id)
		}
		if response.NextPageToken == "" {
			return identifiers, false, nil
		}
		if len(identifiers) == limit {
			return identifiers, true, nil
		}
		if seenPages[response.NextPageToken] {
			return nil, false, fmt.Errorf("Gmail returned a repeated page token")
		}
		pageToken = response.NextPageToken
		seenPages[pageToken] = true
	}
}

func fetchEmail(ctx context.Context, service *gmail.Service, identifier string, maxCharacters int) (emailMessage, error) {
	message, err := service.Users.Messages.Get("me", identifier).Format("full").Context(ctx).Do()
	if err != nil {
		return emailMessage{}, err
	}
	loadBody := func(attachmentID string) (string, error) {
		attachment, err := service.Users.Messages.Attachments.Get("me", identifier, attachmentID).Context(ctx).Do()
		if err != nil {
			return "", err
		}
		return attachment.Data, nil
	}
	return normalizeEmail(message, maxCharacters, loadBody)
}

func normalizeEmail(message *gmail.Message, maxCharacters int, loadBody func(string) (string, error)) (emailMessage, error) {
	result := emailMessage{ID: message.Id, ThreadID: message.ThreadId, Received: time.UnixMilli(message.InternalDate).UTC()}
	if message.Payload == nil || maxCharacters < 1 {
		return result, fmt.Errorf("missing payload or invalid body limit")
	}
	for _, header := range message.Payload.Headers {
		switch strings.ToLower(header.Name) {
		case "from":
			result.From = header.Value
		case "subject":
			result.Subject = header.Value
		}
	}
	body, err := extractBody(message.Payload, loadBody)
	if err != nil {
		return result, err
	}
	result.Body = strings.TrimSpace(body)
	if result.Body == "" {
		return result, fmt.Errorf("message has no readable text body")
	}
	characters := []rune(result.Body)
	if len(characters) > maxCharacters {
		result.Truncated = true
		result.Body = string(characters[:maxCharacters])
	}
	return result, nil
}

func extractBody(part *gmail.MessagePart, loadBody func(string) (string, error)) (string, error) {
	contentType := part.MimeType
	disposition := ""
	for _, header := range part.Headers {
		if strings.EqualFold(header.Name, "Content-Type") {
			contentType = header.Value
		}
		if strings.EqualFold(header.Name, "Content-Disposition") {
			disposition = header.Value
		}
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", fmt.Errorf("invalid MIME content type: %w", err)
	}
	if part.Filename != "" || strings.HasPrefix(strings.ToLower(disposition), "attachment") {
		return "", nil
	}
	if strings.HasPrefix(mediaType, "multipart/") {
		if mediaType == "multipart/alternative" {
			for _, child := range part.Parts {
				if strings.EqualFold(child.MimeType, "text/plain") {
					text, err := extractBody(child, loadBody)
					if err != nil || strings.TrimSpace(text) != "" {
						return text, err
					}
				}
			}
		}
		var bodies []string
		for _, child := range part.Parts {
			text, err := extractBody(child, loadBody)
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(text) != "" {
				bodies = append(bodies, text)
				if mediaType == "multipart/alternative" {
					break
				}
			}
		}
		return strings.Join(bodies, "\n"), nil
	}
	if (mediaType != "text/plain" && mediaType != "text/html") || part.Body == nil {
		return "", nil
	}
	data := part.Body.Data
	if data == "" && part.Body.AttachmentId != "" {
		if loadBody == nil {
			return "", fmt.Errorf("text body requires attachment retrieval")
		}
		data, err = loadBody(part.Body.AttachmentId)
		if err != nil {
			return "", fmt.Errorf("text body retrieval failed: %w", err)
		}
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(data, "="))
	if err != nil {
		return "", fmt.Errorf("invalid Gmail body encoding: %w", err)
	}
	reader, err := charset.NewReader(strings.NewReader(string(decoded)), contentType)
	if err != nil {
		return "", fmt.Errorf("unsupported body charset: %w", err)
	}
	textBytes, err := io.ReadAll(reader)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(textBytes) {
		return "", fmt.Errorf("body is not valid UTF-8")
	}
	if mediaType == "text/html" {
		return htmlText(string(textBytes))
	}
	return string(textBytes), nil
}

func htmlText(source string) (string, error) {
	document, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return "", err
	}
	var output strings.Builder
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && (node.Data == "script" || node.Data == "style" || node.Data == "head") {
			return
		}
		if node.Type == html.TextNode {
			output.WriteString(node.Data)
		}
		if node.Type == html.ElementNode && (node.Data == "br" || node.Data == "p" || node.Data == "div" || node.Data == "li" || node.Data == "tr" || node.Data == "blockquote") {
			output.WriteByte('\n')
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
		if node.Type == html.ElementNode && (node.Data == "p" || node.Data == "div" || node.Data == "li" || node.Data == "tr" || node.Data == "blockquote") {
			output.WriteByte('\n')
		}
	}
	walk(document)
	return strings.TrimSpace(output.String()), nil
}
