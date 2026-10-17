# Gmail Job Progress Reader

A read-only Go CLI that searches Gmail INBOX messages, including read messages,
for job-related keywords and classifies candidates with Google's Gemini API.
Offers, interview invitations/scheduling, and explicit next-round advancement
count as positive. Rejections, acknowledgments, and generic recruiter outreach
do not. Ambiguous or truncated messages are displayed separately for review.

## Prerequisites

- Go 1.27.1 or newer.
- A Google Cloud project with the Gmail API enabled and a desktop OAuth client.
- Save the downloaded OAuth client configuration as `credentials.json` in the
  project directory. This file is private and must not be uploaded.
- A Gemini API key from https://aistudio.google.com/apikey for classification.

On the first run, follow the Gmail authorization prompt. The resulting
`token.json` is stored locally and uses the Gmail read-only scope. Both files
are excluded by `.gitignore`, along with local build outputs and `.env` files.
Never commit credentials, tokens, or API keys. If a secret has already been
committed or uploaded, remove it from the repository and revoke or rotate it;
deleting it from the latest revision does not remove it from Git history.

## Run

Run these commands from the project directory. Use `go run .`, not
`go run quickstart.go`, because the program spans multiple source files.

```sh
go run . -help
go run . -dry-run -limit 10
```

The dry run searches Gmail and prints candidate headers only. It does not
retrieve bodies or contact Gemini. It still needs Gmail OAuth authorization.

Set `GEMINI_API_KEY` locally (PowerShell: `$env:GEMINI_API_KEY = 'your-key'`;
macOS/Linux: `export GEMINI_API_KEY='your-key'`). Do not commit it or paste it
into chat. Then explicitly authorize sending candidate email text:

```sh
go run . -send-to-gemini -limit 10
```

Defaults: 30 days, 200 candidates, 16,000 body characters, a 45-second timeout
per Gmail fetch or Gemini classification, and `gemini-3.5-flash-lite`.
The model must be available to your Gemini account; override it as needed:

```powershell
go run . -send-to-gemini -days 30 -limit 100 -model gemini-3.8-flash
go run . -send-to-gemini -keywords 'interview,offer,next step,next round'
```

`GEMINI_MODEL` sets the model default. Flags override it. Keywords may contain
letters, digits, spaces, and hyphens; separate them with commas.

Search excludes the automated alert addresses `jobalerts-noreply@linkedin.com`
and `jobalert@lensa.com` by default, before applying the candidate limit.
This does not exclude all LinkedIn mail or personal recruiter messages.
Use `-include-job-alerts` to include those alerts again:

```powershell
go run . -dry-run -limit 10 -include-job-alerts
```

## Output And Limits

Positive results show category, sender, subject, date, message ID, and any
explicit company, role, next action, deadline, explanation, and evidence.
Deadlines are shown as written, without inferred dates. Uncertain messages
appear in a separate review group. The summary reports candidates, visited,
classified, positive, non-positive, uncertain, failed, truncated, and remaining
counts. Dry runs leave classification counts at zero.

Individual fetch or classification failures do not prevent other messages from
being processed. An incomplete or cancelled run exits unsuccessfully with a
summary. Permanent Gemini configuration/authentication errors and exhausted
rate limits stop processing to avoid repeated failing requests. Transient
Gemini 429/500/502/503/504 responses have bounded retries within the timeout.

A cap warning means the scan is partial. Raise `-limit` to scan more. Each
message is deduplicated within a run; there is no cross-run cache, so reruns
can incur API charges again. There is no persisted email report.

The keyword gate can miss relevant messages without a matching term. INBOX-only
search excludes archived mail and normally excludes spam/trash. Attachments
are not analyzed. Other newsletter senders can still match; the alert exclusion
is limited to the two known addresses above. Earlier replies remain as context rather than being deleted
heuristically; Gemini is instructed to classify the current reply, not a quoted
historical offer. Messages needing unavailable thread context can be uncertain.
Messages with missing/unreadable bodies remain unclassified, not positive.

## Privacy And Reliability

`-send-to-gemini` sends the candidate sender, subject, and normalized body text
to Google's API. Consider the sensitivity of your emails and review the terms
and data handling applicable to your account:
https://ai.google.dev/gemini-api/terms

The program never changes Gmail labels/read status, deletes mail, or sends
replies. It does not give Gemini tools or Gmail credentials. Email content is
untrusted data; embedded instructions must not control classification.
Structured results are validated, and positive classifications require an
evidence quote present in the input. This checks grounding, not semantic
correctness: AI classifications, company/role extraction, and next actions
remain advisory and should be reviewed. Do not log raw email bodies or keys.

## Verify

Automated tests use local HTTP servers, not Gmail credentials or paid APIs.

```powershell
gofmt -w quickstart.go gmail.go scan.go classifier.go gmail_test.go classifier_test.go
go test ./... -timeout 30s
go vet ./...
go build ./...
```

Before relying on results, evaluate a small authorized live scan against known
offers, interview invitations, next rounds, acknowledgments, rejections,
generic outreach, and an old offer quoted in a newer rejection. Mocked tests
verify parsing and pipeline behavior, not Gemini's real classification accuracy.