package engine

import (
	"errors"
	"fmt"
	"net/url"
	"testing"
)

// A failure must say why, without the RPC URL (and its API key) that a
// *url.Error carries.
func TestErrorTextKeepsCauseAndDropsRequestURL(t *testing.T) {
	request := &url.Error{Op: "Post", URL: "https://rpc.example/?api-key=secret", Err: errors.New("context deadline exceeded")}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errors.New("lookup shared catalog head is unavailable"), "lookup shared catalog head is unavailable"},
		{fmt.Errorf("lookup blockhash: %w", request), "lookup blockhash: Post: context deadline exceeded"},
		{&url.Error{Op: "Get", URL: "https://rpc.example/?api-key=secret", Err: errors.New(`failed to parse Location header "https://x/?api-key=SECRET"`)}, `Get: failed to parse Location header "<url>`},
	} {
		if got := ErrorText(tc.err); got != tc.want {
			t.Errorf("ErrorText(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
}
