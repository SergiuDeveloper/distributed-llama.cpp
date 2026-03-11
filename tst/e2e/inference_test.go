package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

func baseURL() string {
	if v := os.Getenv("SERVER_ADDR"); v != "" {
		return v
	}
	return "http://localhost:8181"
}

func TestInference(t *testing.T) {
	host := baseURL()

	healthResp, err := http.Get(host + "/health")
	if err != nil {
		t.Fatalf("health check failed: %v", err)
	}
	healthBody, _ := io.ReadAll(healthResp.Body)
	healthResp.Body.Close()
	t.Logf("health: %s", healthBody)

	client := openai.NewClient(
		option.WithBaseURL(host+"/v1/"),
		option.WithAPIKey("none"),
	)

	resp, err := client.Chat.Completions.New(context.Background(), openai.ChatCompletionNewParams{
		Model: "local",
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.UserMessage("You okay?"),
		},
	})
	if err != nil {
		t.Fatalf("inference failed: %v", err)
	}

	if len(resp.Choices) == 0 {
		t.Fatal("no choices in response")
	}

	content := resp.Choices[0].Message.Content
	if len(content) < 1 {
		t.Fatal("response content is empty")
	}

	fmt.Printf("\n--- model response ---\n%s\n----------------------\n", content)
}
