package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	summaryLLMTries   = 4
	summaryLLMTimeout = 2 * time.Minute
)

type summaryLLMStatus struct{ code int }

func (e summaryLLMStatus) Error() string { return fmt.Sprintf("redpill: HTTP %d", e.code) }

func (r *Redpill) complete(ctx context.Context, model, system, prompt string) (string, error) {
	body, err := json.Marshal(completionRequest{Model: model, Messages: []chatMessage{{Role: "system", Content: system}, {Role: "user", Content: prompt}}})
	if err != nil {
		return "", err
	}
	var last error
	for try := 0; try < summaryLLMTries; try++ {
		content, err := r.post(ctx, body)
		if err == nil {
			return content, nil
		}
		last = err
		if status, ok := err.(summaryLLMStatus); ok && status.code < 500 {
			return "", err
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	return "", last
}

func (r *Redpill) post(ctx context.Context, body []byte) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, summaryLLMTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("redpill: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+r.key)
	response, err := r.client.Do(request)
	if err != nil {
		return "", fmt.Errorf("redpill: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("redpill: read: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return "", summaryLLMStatus{response.StatusCode}
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &completion); err != nil {
		return "", fmt.Errorf("redpill: unreadable completion: %w", err)
	}
	if len(completion.Choices) == 0 {
		return "", fmt.Errorf("redpill: completion has no choices")
	}
	return completion.Choices[0].Message.Content, nil
}
