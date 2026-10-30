package deepseek

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClassifyDomainsBuildsDomainOnlyRequest(t *testing.T) {
	var seen map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Errorf("decode request: %v", err)
		}
		content := `{"results":[
			{"domain":"slither.io","isGame":true,"name":"Slither","confidence":1.4,"reason":"Browser game."},
			{"domain":"unknown.example","isGame":false,"name":"","confidence":-1,"reason":"Not a game."}
		]}`
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
	}))
	defer server.Close()

	client := NewClientWithEndpoint(server.URL)
	verdicts, err := client.ClassifyDomains(context.Background(), "sk-test", "deepseek-flash", []DomainObservation{
		{Domain: "slither.io"},
		{Domain: "unknown.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 2 {
		t.Fatalf("verdicts = %+v", verdicts)
	}
	if verdicts[0].Confidence != 1 {
		t.Fatalf("confidence was not clamped: %+v", verdicts[0])
	}
	messages := seen["messages"].([]any)
	userMessage := messages[1].(map[string]any)["content"].(string)
	if !strings.Contains(userMessage, "slither.io") {
		t.Fatalf("user message missing domain: %s", userMessage)
	}
	if strings.Contains(userMessage, "/") || strings.Contains(userMessage, "http") {
		t.Fatalf("user message contains URL data: %s", userMessage)
	}
}

func TestClassifyDomainsIgnoresUnknownAndDuplicateResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		content := `{"results":[
			{"domain":"not-sent.example","isGame":true,"confidence":0.99},
			{"domain":"sent.example","isGame":true,"confidence":0.8},
			{"domain":"sent.example","isGame":false,"confidence":0.1}
		]}`
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
	}))
	defer server.Close()

	client := NewClientWithEndpoint(server.URL)
	verdicts, err := client.ClassifyDomains(context.Background(), "sk-test", "deepseek-flash", []DomainObservation{
		{Domain: "sent.example"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 1 || verdicts[0].Domain != "sent.example" || verdicts[0].Confidence != 0.8 {
		t.Fatalf("verdicts = %+v", verdicts)
	}
}

func TestClassifyDomainsRejectsOversizedBatch(t *testing.T) {
	client := NewClientWithEndpoint("http://127.0.0.1:1")
	observations := make([]DomainObservation, maxBatchSize+1)
	if _, err := client.ClassifyDomains(context.Background(), "sk-test", "deepseek-flash", observations); err == nil {
		t.Fatal("expected an error")
	}
}
