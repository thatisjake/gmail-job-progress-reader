package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

// Retrieve a token, saves the token, then returns the generated client.
func getClient(config *oauth2.Config) *http.Client {
	// The file token.json stores the user's access and refresh tokens, and is
	// created automatically when the authorization flow completes for the first
	// time.
	tokFile := "token.json"
	tok, err := tokenFromFile(tokFile)
	if err != nil {
		tok = getTokenFromWeb(config)
		saveToken(tokFile, tok)
	}
	return config.Client(context.Background(), tok)
}

// Request a token from the web, then returns the retrieved token.
func getTokenFromWeb(config *oauth2.Config) *oauth2.Token {
	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Go to the following link in your browser then type the "+
		"authorization code: \n%v\n", authURL)

	var authCode string
	if _, err := fmt.Scan(&authCode); err != nil {
		log.Fatalf("Unable to read authorization code: %v", err)
	}

	tok, err := config.Exchange(context.TODO(), authCode)
	if err != nil {
		log.Fatalf("Unable to retrieve token from web: %v", err)
	}
	return tok
}

// Retrieves a token from a local file.
func tokenFromFile(file string) (*oauth2.Token, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tok := &oauth2.Token{}
	err = json.NewDecoder(f).Decode(tok)
	return tok, err
}

// Saves a token to a file path.
func saveToken(path string, token *oauth2.Token) {
	fmt.Printf("Saving credential file to: %s\n", path)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		log.Fatalf("Unable to cache oauth token: %v", err)
	}
	defer f.Close()
	json.NewEncoder(f).Encode(token)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := runApplication(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", terminalText(err.Error()))
		os.Exit(1)
	}
}

func runApplication(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("quickstart", flag.ContinueOnError)
	flags.SetOutput(output)
	days := flags.Int("days", 30, "Look back this many days in INBOX (read and unread)")
	keywords := flags.String("keywords", defaultKeywords, "Comma-separated keyword search terms")
	limit := flags.Int("limit", 200, "Maximum number of candidate messages")
	modelDefault := strings.TrimSpace(os.Getenv("GEMINI_MODEL"))
	if modelDefault == "" {
		modelDefault = defaultModel
	}
	model := flags.String("model", modelDefault, "Gemini model (or set GEMINI_MODEL)")
	bodyLimit := flags.Int("max-body-chars", 16000, "Maximum body characters sent per message; truncated messages remain uncertain")
	timeout := flags.Duration("timeout", 45*time.Second, "Timeout per Gmail fetch or Gemini classification, including Gemini retries")
	dryRun := flags.Bool("dry-run", false, "List candidate metadata without contacting Gemini")
	includeJobAlerts := flags.Bool("include-job-alerts", false, "Include automated alerts from LinkedIn Job Alerts and Lensa")
	sendToGemini := flags.Bool("send-to-gemini", false, "Consent to sending candidate sender, subject, and body text to Google's Gemini API")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments; use -help")
	}
	query, err := buildQuery(*days, *keywords, *includeJobAlerts)
	if err != nil {
		return err
	}
	if *limit < 1 || *bodyLimit < 1 || *timeout <= 0 {
		return fmt.Errorf("limit, max-body-chars, and timeout must be positive")
	}
	var classifier emailClassifier
	if !*dryRun {
		if !*sendToGemini {
			return fmt.Errorf("use -dry-run to preview, or -send-to-gemini to allow email text to be sent to Google; API charges may apply")
		}
		classifier, err = newGeminiClassifier(ctx, os.Getenv("GEMINI_API_KEY"), *model, *timeout)
		if err != nil {
			return err
		}
	}
	b, err := os.ReadFile("credentials.json")
	if err != nil {
		return fmt.Errorf("unable to read Gmail credentials; run from the quickstart project directory")
	}

	// If modifying these scopes, delete your previously saved token.json.
	config, err := google.ConfigFromJSON(b, gmail.GmailReadonlyScope)
	if err != nil {
		return fmt.Errorf("unable to parse Gmail client configuration")
	}
	client := getClient(config)
	client.Timeout = *timeout

	srv, err := gmail.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return fmt.Errorf("unable to initialize Gmail client")
	}
	if *dryRun {
		fmt.Fprintln(output, "Dry run: no email text will be sent to Gemini.")
	} else {
		fmt.Fprintf(output, "Gemini model: %s. Candidate email text is sent to Google; API charges may apply.\n", terminalText(*model))
	}
	if !*includeJobAlerts {
		fmt.Fprintln(output, "Excluding known LinkedIn/Lensa job-alert senders. Use -include-job-alerts to include them.")
	}
	return runScan(ctx, srv, classifier, scanSettings{
		query: query, limit: *limit, bodyLimit: *bodyLimit, timeout: *timeout, dryRun: *dryRun,
	}, output)
}
