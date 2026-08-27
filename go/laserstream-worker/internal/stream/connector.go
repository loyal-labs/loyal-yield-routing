package stream

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/url"
	"strings"
	"time"

	pb "github.com/helius-labs/laserstream-sdk/go/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
)

const (
	defaultMaxReceiveMessageSize = 1_000_000_000
	defaultMaxSendMessageSize    = 32_000_000
)

// ClientStream is the bidirectional portion of a Yellowstone subscription used
// by Manager. The narrow interface also lets the handoff contract be tested
// against an in-process gRPC server.
type ClientStream interface {
	Recv() (*pb.SubscribeUpdate, error)
	Send(*pb.SubscribeRequest) error
	CloseSend() error
}

// OpenStream owns both the stream and its underlying connection.
type OpenStream interface {
	ClientStream
	Close() error
}

// Connector opens one physical LaserStream subscription.
type Connector interface {
	Open(context.Context, *pb.SubscribeRequest) (OpenStream, error)
}

// GRPCConnector connects directly to Helius using the official protobuf
// surface. Loyal owns reconnection and replay instead of using the SDK's
// receive-side slot tracker, which can advance ahead of durable processing.
type GRPCConnector struct {
	Endpoint    string
	APIKey      string
	DialOptions []grpc.DialOption
}

func (c GRPCConnector) Open(ctx context.Context, request *pb.SubscribeRequest) (OpenStream, error) {
	target, err := grpcTarget(c.Endpoint)
	if err != nil {
		return nil, err
	}

	opts := c.DialOptions
	if len(opts) == 0 {
		opts = []grpc.DialOption{
			grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})),
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time:                30 * time.Second,
				Timeout:             5 * time.Second,
				PermitWithoutStream: true,
			}),
			grpc.WithInitialWindowSize(4 * 1024 * 1024),
			grpc.WithInitialConnWindowSize(8 * 1024 * 1024),
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(defaultMaxReceiveMessageSize),
				grpc.MaxCallSendMsgSize(defaultMaxSendMessageSize),
			),
		}
	}

	conn, err := grpc.NewClient(target, opts...)
	if err != nil {
		return nil, fmt.Errorf("create LaserStream client: %w", err)
	}

	streamCtx := ctx
	if c.APIKey != "" {
		streamCtx = metadata.NewOutgoingContext(streamCtx, metadata.Pairs(
			"x-token", c.APIKey,
			"x-sdk-name", "loyal-laserstream-worker",
		))
	}
	client, err := pb.NewGeyserClient(conn).Subscribe(streamCtx)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("open LaserStream subscription: %w", err)
	}
	if err := client.Send(request); err != nil {
		_ = client.CloseSend()
		_ = conn.Close()
		return nil, fmt.Errorf("send LaserStream subscription request: %w", err)
	}
	return &grpcOpenStream{ClientStream: client, conn: conn}, nil
}

type grpcOpenStream struct {
	ClientStream
	conn *grpc.ClientConn
}

func (s *grpcOpenStream) Close() error {
	_ = s.CloseSend()
	return s.conn.Close()
}

func grpcTarget(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", fmt.Errorf("LaserStream endpoint is required")
	}
	if strings.HasPrefix(endpoint, "passthrough:///") || strings.HasPrefix(endpoint, "dns:///") {
		return endpoint, nil
	}
	if !strings.Contains(endpoint, "://") {
		if strings.Contains(endpoint, ":") {
			return endpoint, nil
		}
		return endpoint + ":443", nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse LaserStream endpoint: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("LaserStream endpoint has no host: %q", endpoint)
	}
	if u.Port() != "" {
		return u.Host, nil
	}
	if u.Scheme == "http" {
		return u.Hostname() + ":80", nil
	}
	return u.Hostname() + ":443", nil
}
