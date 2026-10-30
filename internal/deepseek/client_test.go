package deepseek

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClassifyBuildsRequestAndParsesVerdicts(t *testing.T) {
	var seen map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content-type = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&seen); err != nil {
			t.Errorf("decode request: %v", err)
		}
		content := `{"results":[{"executable":"game.exe","imagePath":"C:\\Games\\game.exe","isGame":true,"name":"Example Game","confidence":0.91,"reason":"Interactive game client."}]}`
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
	}))
	defer server.Close()

	client := NewClientWithEndpoint(server.URL)
	verdicts, err := client.Classify(context.Background(), "sk-test", "deepseek-flash", []Observation{{
		Executable: "game.exe",
		ImagePath:  `C:\Games\game.exe`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 1 || verdicts[0].Name != "Example Game" || !verdicts[0].IsGame {
		t.Fatalf("unexpected verdicts: %+v", verdicts)
	}
	if seen["model"] != "deepseek-flash" {
		t.Fatalf("model = %v", seen["model"])
	}
	if seen["stream"] != false {
		t.Fatalf("stream = %v", seen["stream"])
	}
	format := seen["response_format"].(map[string]any)
	if format["type"] != "json_object" {
		t.Fatalf("response format = %+v", format)
	}
	thinking := seen["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Fatalf("thinking = %+v", thinking)
	}
	messages := seen["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %+v", messages)
	}
	userMessage := messages[1].(map[string]any)["content"].(string)
	if !strings.Contains(userMessage, `"game.exe"`) {
		t.Fatalf("user message did not contain process data: %s", userMessage)
	}
}

func TestClassifyIgnoresUnmatchedResultsAndBoundsText(t *testing.T) {
	content := `{"results":[
		{"executable":"other.exe","imagePath":"C:\\Other\\other.exe","isGame":true,"name":"Other","confidence":2,"reason":"x"},
		{"executable":"game.exe","imagePath":"C:\\Games\\game.exe","isGame":false,"name":"","confidence":-1,"reason":"not a game"}
	]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		})
	}))
	defer server.Close()

	client := NewClientWithEndpoint(server.URL)
	verdicts, err := client.Classify(context.Background(), "sk-test", "deepseek-flash", []Observation{{
		Executable: "game.exe",
		ImagePath:  `C:\Games\game.exe`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(verdicts) != 1 {
		t.Fatalf("unexpected verdicts: %+v", verdicts)
	}
	if verdicts[0].Confidence != 0 {
		t.Fatalf("confidence was not clamped: %+v", verdicts[0])
	}
}

func TestClassifyRejectsProviderAuthErrorWithoutLeakingKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer server.Close()

	client := NewClientWithEndpoint(server.URL)
	_, err := client.Classify(context.Background(), "sk-secret-key", "deepseek-flash", []Observation{{
		Executable: "game.exe",
	}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "sk-secret-key") {
		t.Fatalf("error leaked the API key: %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v", err)
	}
}

func TestClassifyRejectsMalformedContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "not-json"}}},
		})
	}))
	defer server.Close()

	client := NewClientWithEndpoint(server.URL)
	_, err := client.Classify(context.Background(), "sk-test", "deepseek-flash", []Observation{{Executable: "game.exe"}})
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestClassifyRejectsOversizedBatch(t *testing.T) {
	client := NewClientWithEndpoint("http://127.0.0.1:1")
	observations := make([]Observation, maxBatchSize+1)
	if _, err := client.Classify(context.Background(), "sk-test", "deepseek-flash", observations); err == nil {
		t.Fatal("expected an error")
	}
}
