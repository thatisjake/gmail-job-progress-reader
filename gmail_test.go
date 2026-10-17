package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

func testGmailService(t *testing.T, handler http.HandlerFunc) *gmail.Service {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	service, err := gmail.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestBuildQuery(t *testing.T) {
	query, err := buildQuery(30, "interview,next step,next round", false)
	want := `newer_than:30d {"interview" "next step" "next round"} -from:jobalerts-noreply@linkedin.com -from:jobalert@lensa.com`
	if err != nil || query != want {
		t.Fatalf("query = %q, error = %v", query, err)
	}
	query, err = buildQuery(30, "interview,next step,next round", true)
	if err != nil || query != `newer_than:30d {"interview" "next step" "next round"}` {
		t.Fatalf("opt-in query = %q, error = %v", query, err)
	}
	for _, keywords := range []string{"", ", ,", "interview\" OR in:anywhere", "subject:offer"} {
		if _, err := buildQuery(30, keywords, false); err == nil {
			t.Errorf("accepted invalid keywords %q", keywords)
		}
	}
	if _, err := buildQuery(0, "interview", false); err == nil {
		t.Fatal("accepted invalid lookback")
	}
}

func TestFindCandidates(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		limit   int
		want    string
		partial bool
	}{
		{"pagination", 10, "first,second,third", false},
		{"cap", 2, "first,second", true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			service := testGmailService(t, func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Query().Get("labelIds") != "INBOX" || request.URL.Query().Get("q") != "query" {
					t.Errorf("unexpected query: %s", request.URL.RawQuery)
				}
				if strings.Contains(request.URL.Query().Get("q"), "is:unread") {
					t.Error("scan unexpectedly restricts read status")
				}
				writer.Header().Set("Content-Type", "application/json")
				if request.URL.Query().Get("pageToken") == "" {
					fmt.Fprint(writer, `{"messages":[{"id":"first"},{"id":"second"}],"nextPageToken":"next"}`)
				} else {
					fmt.Fprint(writer, `{"messages":[{"id":"second"},{"id":"third"}]}`)
				}
			})
			identifiers, partial, err := findCandidates(context.Background(), service, "query", testCase.limit)
			if err != nil || strings.Join(identifiers, ",") != testCase.want || partial != testCase.partial {
				t.Fatalf("ids=%v partial=%v error=%v", identifiers, partial, err)
			}
		})
	}
}

func textPart(mediaType, text string) *gmail.MessagePart {
	return &gmail.MessagePart{MimeType: mediaType, Body: &gmail.MessagePartBody{Data: base64.RawURLEncoding.EncodeToString([]byte(text))}}
}

func TestNormalizeEmail(t *testing.T) {
	plain := textPart("text/plain", "Interview invitation\n> Previous rejection")
	htmlPart := textPart("text/html", "<style>hidden</style><p>Hello &amp; welcome</p><p>Next round</p>")
	file := textPart("text/plain", "ignore this attachment")
	file.Filename = "notes.txt"
	for _, testCase := range []struct {
		name    string
		payload *gmail.MessagePart
		want    string
	}{
		{"plain with quoted context", plain, "Interview invitation\n> Previous rejection"},
		{"html", htmlPart, "Hello & welcome\n\nNext round"},
		{"nested alternatives", &gmail.MessagePart{MimeType: "multipart/mixed", Parts: []*gmail.MessagePart{
			{MimeType: "multipart/alternative", Parts: []*gmail.MessagePart{htmlPart, plain}}, file,
		}}, "Interview invitation\n> Previous rejection"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			message, err := normalizeEmail(&gmail.Message{Id: "id", Payload: testCase.payload}, 1000, nil)
			if err != nil || message.Body != testCase.want {
				t.Fatalf("body=%q error=%v", message.Body, err)
			}
		})
	}
}

func TestBodyEdgeCases(t *testing.T) {
	part := textPart("text/plain", "abcdef")
	part.Body.Data = base64.URLEncoding.EncodeToString([]byte("abcdef"))
	message, err := normalizeEmail(&gmail.Message{Payload: part}, 3, nil)
	if err != nil || message.Body != "abc" || !message.Truncated {
		t.Fatalf("unexpected truncation: %+v, %v", message, err)
	}
	part = &gmail.MessagePart{MimeType: "text/plain", Body: &gmail.MessagePartBody{AttachmentId: "body-id"}}
	message, err = normalizeEmail(&gmail.Message{Payload: part}, 100, func(identifier string) (string, error) {
		if identifier != "body-id" {
			t.Fatal("wrong body attachment ID")
		}
		return base64.RawURLEncoding.EncodeToString([]byte("next round")), nil
	})
	if err != nil || message.Body != "next round" {
		t.Fatalf("body retrieval: %+v, %v", message, err)
	}
	part = textPart("text/plain", "caf\xe9")
	part.Headers = []*gmail.MessagePartHeader{{Name: "Content-Type", Value: "text/plain; charset=iso-8859-1"}}
	message, err = normalizeEmail(&gmail.Message{Payload: part}, 100, nil)
	if err != nil || message.Body != "caf\u00e9" {
		t.Fatalf("charset decoding: %+v, %v", message, err)
	}
	if _, err := normalizeEmail(&gmail.Message{Payload: textPart("text/plain", "")}, 100, nil); err == nil {
		t.Fatal("empty body accepted")
	}
	part.Body.Data = "invalid!"
	if _, err := normalizeEmail(&gmail.Message{Payload: part}, 100, nil); err == nil {
		t.Fatal("invalid encoding accepted")
	}
}
