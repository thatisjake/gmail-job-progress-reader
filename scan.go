package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"

	"google.golang.org/api/gmail/v1"
)

type scanSettings struct {
	query     string
	limit     int
	bodyLimit int
	timeout   time.Duration
	dryRun    bool
}

type reviewResult struct {
	message emailMessage
	result  classification
}

func terminalText(text string) string {
	return strings.Join(strings.Fields(strings.Map(func(character rune) rune {
		if unicode.IsControl(character) || unicode.In(character, unicode.Cf) {
			return ' '
		}
		return character
	}, text)), " ")
}

func printResult(output io.Writer, message emailMessage, result classification) {
	fmt.Fprintf(output, "\n[%s] %s\nFrom: %s\nDate: %s | Message ID: %s\n",
		terminalText(result.Category), terminalText(message.Subject), terminalText(message.From),
		message.Received.Format(time.RFC3339), terminalText(message.ID))
	for _, field := range []struct{ name, value string }{
		{"Company", result.Company}, {"Role", result.Role}, {"Next action", result.NextAction},
		{"Deadline (as written)", result.Deadline}, {"Reason", result.Reason}, {"Evidence", result.Evidence},
	} {
		if field.value != "" {
			fmt.Fprintf(output, "%s: %s\n", field.name, terminalText(field.value))
		}
	}
}

func runScan(ctx context.Context, service *gmail.Service, classifier emailClassifier, settings scanSettings, output io.Writer) error {
	identifiers, partial, err := findCandidates(ctx, service, settings.query, settings.limit)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Gmail search failed; check authentication and connectivity")
	}
	fmt.Fprintf(output, "Found %d keyword candidates in INBOX.\n", len(identifiers))
	if partial {
		fmt.Fprintln(output, "PARTIAL SCAN: candidate limit reached; more messages may remain. Increase -limit to scan more.")
	}
	if len(identifiers) == 0 {
		fmt.Fprintln(output, "No matches. No Gemini requests made.")
		return nil
	}
	visited, classified, positives, nonPositives, failed, truncated := 0, 0, 0, 0, 0, 0
	var uncertain []reviewResult
	defer func() {
		if len(uncertain) > 0 {
			fmt.Fprintln(output, "\nNeeds manual review:")
			for _, review := range uncertain {
				printResult(output, review.message, review.result)
			}
		}
		fmt.Fprintf(output, "\nSummary: candidates=%d visited=%d classified=%d positive=%d non-positive=%d uncertain=%d failed=%d truncated=%d remaining=%d\n",
			len(identifiers), visited, classified, positives, nonPositives, len(uncertain), failed, truncated, len(identifiers)-visited)
	}()
	for _, identifier := range identifiers {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		visited++
		fetchCtx, cancel := context.WithTimeout(ctx, settings.timeout)
		if settings.dryRun {
			message, fetchErr := service.Users.Messages.Get("me", identifier).Format("metadata").MetadataHeaders("From", "Subject").Context(fetchCtx).Do()
			cancel()
			if fetchErr != nil || message.Payload == nil {
				failed++
				fmt.Fprintf(output, "Unable to fetch candidate metadata: %s\n", terminalText(identifier))
				continue
			}
			preview := emailMessage{ID: identifier, Received: time.UnixMilli(message.InternalDate).UTC()}
			for _, header := range message.Payload.Headers {
				if strings.EqualFold(header.Name, "From") {
					preview.From = header.Value
				}
				if strings.EqualFold(header.Name, "Subject") {
					preview.Subject = header.Value
				}
			}
			printResult(output, preview, classification{Category: "candidate; not classified"})
			continue
		}
		message, fetchErr := fetchEmail(fetchCtx, service, identifier, settings.bodyLimit)
		cancel()
		if fetchErr != nil {
			failed++
			fmt.Fprintf(output, "Unable to fetch a readable email body: %s (unclassified)\n", terminalText(identifier))
			continue
		}
		if message.Truncated {
			truncated++
		}
		result, classifyErr := classifier.Classify(ctx, message)
		if classifyErr != nil {
			failed++
			fmt.Fprintf(output, "Unable to classify message %s: %s\n", terminalText(identifier), terminalText(classifyErr.Error()))
			if ctx.Err() != nil {
				return ctx.Err()
			}
			var requestError geminiRequestError
			if errors.As(classifyErr, &requestError) && (requestError.status == 400 || requestError.status == 401 || requestError.status == 403 || requestError.status == 404 || requestError.status == 429) {
				return classifyErr
			}
			continue
		}
		classified++
		if result.positive() {
			if positives == 0 {
				fmt.Fprintln(output, "\nPositive job progress:")
			}
			positives++
			printResult(output, message, result)
		} else if result.Category == "uncertain" {
			uncertain = append(uncertain, reviewResult{message: message, result: result})
		} else {
			nonPositives++
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if failed > 0 {
		return fmt.Errorf("scan incomplete: %d messages could not be processed", failed)
	}
	return nil
}
