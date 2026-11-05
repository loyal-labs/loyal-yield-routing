package webapp

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
)

// POST /api/kamino/klend/{deposit,withdraw}-instructions port the two Next
// passthroughs: the body goes to Kamino's ktx API as sent, the answer comes
// back with Kamino's status and content type, and any transport failure is
// the 502 kamino_proxy_failed envelope.
const (
	kaminoKtxBase           = "https://api.kamino.finance/ktx/klend/"
	kaminoProxyTimeout      = 30 * time.Second
	kaminoMaxRequestBytes   = 4_500_000
	kaminoMaxResponseBytes  = 16 << 20
	kaminoProxyFailedBody   = `{"error":{"code":"kamino_proxy_failed","message":"Kamino request failed."}}`
	kaminoDefaultTextHeader = "text/plain;charset=UTF-8"
)

type kaminoBody struct {
	data []byte
	err  error
}

func (b *kaminoBody) Resolve(ctx huma.Context) []error {
	b.data, b.err = io.ReadAll(io.LimitReader(ctx.BodyReader(), kaminoMaxRequestBytes+1))
	return nil
}

type kaminoInput struct {
	Accept      string `header:"Accept"`
	ContentType string `header:"Content-Type"`
	Raw         kaminoBody
}

type kaminoOutput struct {
	Status      int
	ContentType string `header:"Content-Type"`
	Body        []byte
}

type kaminoProxy struct {
	client *http.Client
	base   string
}

func registerKaminoProxies(api huma.API) {
	kaminoProxy{client: &http.Client{Timeout: kaminoProxyTimeout}, base: kaminoKtxBase}.register(api)
}

func (proxy kaminoProxy) register(api huma.API) {
	for _, operation := range []string{"deposit-instructions", "withdraw-instructions"} {
		path := "/api/kamino/klend/" + operation
		huma.Register(api, huma.Operation{
			OperationID: "post-kamino-klend-" + operation,
			Method:      http.MethodPost,
			Path:        path,
			Summary:     "Forward a Kamino klend " + operation + " request",
		}, proxy.forward(operation))
		api.OpenAPI().Paths[path].Post.RequestBody = &huma.RequestBody{Content: map[string]*huma.MediaType{
			"application/json": {Schema: &huma.Schema{Type: "object"}},
		}}
	}
}

func (p kaminoProxy) forward(operation string) func(context.Context, *kaminoInput) (*kaminoOutput, error) {
	return func(ctx context.Context, in *kaminoInput) (*kaminoOutput, error) {
		out, err := p.call(ctx, operation, in)
		if err != nil {
			slog.Error("kamino proxy failed", "operation", operation, "error", err)
			return &kaminoOutput{
				Status:      http.StatusBadGateway,
				ContentType: "application/json",
				Body:        []byte(kaminoProxyFailedBody),
			}, nil
		}
		return out, nil
	}
}

func (p kaminoProxy) call(ctx context.Context, operation string, in *kaminoInput) (*kaminoOutput, error) {
	if in.Raw.err != nil {
		return nil, in.Raw.err
	}
	if len(in.Raw.data) > kaminoMaxRequestBytes {
		return &kaminoOutput{Status: http.StatusRequestEntityTooLarge, ContentType: kaminoDefaultTextHeader, Body: []byte("Request Entity Too Large")}, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+operation, bytes.NewReader(in.Raw.data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", orDefault(in.Accept, "application/json"))
	req.Header.Set("Content-Type", orDefault(in.ContentType, "application/json"))
	response, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, kaminoMaxResponseBytes))
	if err != nil {
		return nil, err
	}
	return &kaminoOutput{
		Status:      response.StatusCode,
		ContentType: orDefault(response.Header.Get("Content-Type"), kaminoDefaultTextHeader),
		Body:        body,
	}, nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
