package telegram

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
)

type Redpill struct {
	base   string
	key    string
	client *http.Client
}

func NewRedpill(base, key string) *Redpill {
	return &Redpill{base: strings.TrimRight(base, "/"), key: key, client: &http.Client{}}
}

type completionRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature float64       `json:"temperature"`
	Stream      bool          `json:"stream"`
}

type completionChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

func (r *Redpill) stream(ctx context.Context, request completionRequest) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		body, err := json.Marshal(request)
		if err != nil {
			yield("", err)
			return
		}
		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			yield("", fmt.Errorf("redpill: %w", err))
			return
		}
		httpRequest.Header.Set("Content-Type", "application/json")
		httpRequest.Header.Set("Authorization", "Bearer "+r.key)
		response, err := r.client.Do(httpRequest)
		if err != nil {
			yield("", fmt.Errorf("redpill: %w", err))
			return
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode > 299 {
			yield("", fmt.Errorf("redpill: HTTP %d", response.StatusCode))
			return
		}
		reader := bufio.NewReader(response.Body)
		for {
			line, readErr := reader.ReadString('\n')
			line = strings.TrimSuffix(line, "\n")
			if data, ok := strings.CutPrefix(line, "data: "); ok && line != "data: [DONE]" {
				var chunk completionChunk
				if json.Unmarshal([]byte(data), &chunk) == nil && len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
					if !yield(chunk.Choices[0].Delta.Content, nil) {
						return
					}
				}
			}
			if errors.Is(readErr, io.EOF) {
				return
			}
			if readErr != nil {
				yield("", fmt.Errorf("redpill stream: %w", readErr))
				return
			}
		}
	}
}
