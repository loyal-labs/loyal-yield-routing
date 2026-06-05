package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/streamingfast/logging"
	"github.com/streamingfast/substreams/client"
	pbsubstreamsrpc "github.com/streamingfast/substreams/pb/sf/substreams/rpc/v2"
	"github.com/streamingfast/substreams/sink"
)

type sessionEvent struct {
	Kind               string `json:"kind"`
	TraceID            string `json:"trace_id"`
	ResolvedStartBlock uint64 `json:"resolved_start_block"`
	LinearHandoffBlock uint64 `json:"linear_handoff_block"`
	MaxParallelWorkers uint64 `json:"max_parallel_workers"`
	ChainHead          uint64 `json:"chain_head"`
}

type progressEvent struct {
	Kind                   string  `json:"kind"`
	HighestContiguousBlock *uint64 `json:"highest_contiguous_block"`
	ProcessedBlocks        uint64  `json:"processed_blocks"`
	TotalBytesRead         uint64  `json:"total_bytes_read"`
	TotalBytesWritten      uint64  `json:"total_bytes_written"`
	CompletedRangeCount    int     `json:"completed_range_count"`
	RunningJobCount        int     `json:"running_job_count"`
}

type blockEvent struct {
	Kind      string  `json:"kind"`
	Block     uint64  `json:"block"`
	Timestamp *string `json:"timestamp"`
	TypeURL   string  `json:"type_url"`
	Value     string  `json:"value"`
}

type undoEvent struct {
	Kind           string `json:"kind"`
	LastValidBlock uint64 `json:"last_valid_block"`
}

type errorEvent struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func main() {
	endpoint := flag.String("endpoint", "", "Substreams gRPC endpoint")
	packagePath := flag.String("package", "", "Substreams .spkg URL or path")
	outputModule := flag.String("module", "", "Substreams output module")
	params := flag.String("params", "", "Substreams params, for example module=value")
	startBlock := flag.Int64("start-block", 0, "inclusive start block")
	stopBlock := flag.Uint64("stop-block", 0, "exclusive stop block")
	limitProcessedBlocks := flag.Uint64("limit-processed-blocks", 0, "processed-block safety limit")
	apiKeyEnvvar := flag.String("api-key-envvar", "SF_API_TOKEN", "environment variable containing the StreamingFast API key")
	productionMode := flag.Bool("production-mode", false, "run in Substreams production mode")
	parallelWorkers := flag.Uint64("parallel-workers", 0, "optional X-Substreams-Parallel-Workers header")
	flag.Parse()

	if err := run(*endpoint, *packagePath, *outputModule, *params, *startBlock, *stopBlock, *limitProcessedBlocks, *apiKeyEnvvar, *productionMode, *parallelWorkers); err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(errorEvent{
			Kind:    "error",
			Message: err.Error(),
		})
		os.Exit(1)
	}
}

func run(endpoint, packagePath, outputModule, params string, startBlock int64, stopBlock uint64, limitProcessedBlocks uint64, apiKeyEnvvar string, productionMode bool, parallelWorkers uint64) error {
	if endpoint == "" {
		return fmt.Errorf("--endpoint is required")
	}
	if packagePath == "" {
		return fmt.Errorf("--package is required")
	}
	if outputModule == "" {
		return fmt.Errorf("--module is required")
	}
	if stopBlock == 0 {
		return fmt.Errorf("--stop-block is required")
	}
	if startBlock < 0 || uint64(startBlock) >= stopBlock {
		return fmt.Errorf("--start-block must be lower than --stop-block")
	}
	apiKey := os.Getenv(apiKeyEnvvar)
	if apiKey == "" {
		return fmt.Errorf("%s is required", apiKeyEnvvar)
	}

	zlog, tracer := logging.PackageLogger("loyal-kamino-historic-data-adapter", "github.com/loyal/kamino-historic-data/substreams-adapter")
	paramsList := []string{}
	if params != "" {
		paramsList = append(paramsList, params)
	}
	pkg, module, moduleHash, err := sink.ReadManifestAndModule(
		packagePath,
		"",
		paramsList,
		outputModule,
		sink.IgnoreOutputModuleType,
		false,
		nil,
		zlog,
	)
	if err != nil {
		return fmt.Errorf("read Substreams package: %w", err)
	}

	mode := sink.SubstreamsModeDevelopment
	if productionMode {
		mode = sink.SubstreamsModeProduction
	}
	extraHeaders := []string{}
	if parallelWorkers > 0 {
		extraHeaders = append(extraHeaders, fmt.Sprintf("X-Substreams-Parallel-Workers:%d", parallelWorkers))
	}
	sinker, err := sink.NewFromConfig(&sink.SinkerConfig{
		Pkg:              pkg,
		OutputModule:     module,
		OutputModuleHash: moduleHash,
		ClientConfig: client.NewSubstreamsClientConfig(client.SubstreamsClientConfigOptions{
			Endpoint:  endpoint,
			AuthToken: apiKey,
			AuthType:  client.ApiKey,
			Agent:     "loyal-kamino-historic-data/0.1",
		}),
		Mode:                 mode,
		LimitProcessedBlocks: limitProcessedBlocks,
		StartBlock:           startBlock,
		StopBlock:            stopBlock,
		MaxRetries:           0,
		BackOff:              backoff.NewExponentialBackOff(),
		ExtraHeaders:         extraHeaders,
		Logger:               zlog,
		Tracer:               tracer,
		Params:               paramsList,
		Network:              pkg.Network,
	})
	if err != nil {
		return fmt.Errorf("configure Substreams sinker: %w", err)
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)

	handler := sink.NewSinkerFullHandlers(
		func(ctx context.Context, data *pbsubstreamsrpc.BlockScopedData, isLive *bool, cursor *sink.Cursor) error {
			if data == nil || data.Output == nil || data.Output.MapOutput == nil || data.Clock == nil {
				return nil
			}
			var timestamp *string
			if data.Clock.Timestamp != nil {
				formatted := data.Clock.Timestamp.AsTime().UTC().Format(time.RFC3339Nano)
				timestamp = &formatted
			}
			return encoder.Encode(blockEvent{
				Kind:      "block",
				Block:     data.Clock.Number,
				Timestamp: timestamp,
				TypeURL:   data.Output.MapOutput.TypeUrl,
				Value:     base64.StdEncoding.EncodeToString(data.Output.MapOutput.Value),
			})
		},
		func(ctx context.Context, undoSignal *pbsubstreamsrpc.BlockUndoSignal, cursor *sink.Cursor) error {
			lastValidBlock := uint64(0)
			if undoSignal != nil && undoSignal.LastValidBlock != nil {
				lastValidBlock = undoSignal.LastValidBlock.Number
			}
			return encoder.Encode(undoEvent{
				Kind:           "undo",
				LastValidBlock: lastValidBlock,
			})
		},
		func(ctx context.Context, req *pbsubstreamsrpc.Request, session *pbsubstreamsrpc.SessionInit) error {
			return encoder.Encode(sessionEvent{
				Kind:               "session",
				TraceID:            session.TraceId,
				ResolvedStartBlock: session.ResolvedStartBlock,
				LinearHandoffBlock: session.LinearHandoffBlock,
				MaxParallelWorkers: session.MaxParallelWorkers,
				ChainHead:          session.ChainHead,
			})
		},
		func(ctx context.Context, progress *pbsubstreamsrpc.ModulesProgress) {
			event := progressEvent{
				Kind:                "progress",
				ProcessedBlocks:     progress.ProcessedBlocks,
				CompletedRangeCount: completedRangeCount(progress),
				RunningJobCount:     len(progress.RunningJobs),
			}
			if progress.ProcessedBytes != nil {
				event.TotalBytesRead = progress.ProcessedBytes.TotalBytesRead
				event.TotalBytesWritten = progress.ProcessedBytes.TotalBytesWritten
			}
			if highest := highestContiguousBlock(progress); highest != 0 {
				event.HighestContiguousBlock = &highest
			}
			_ = encoder.Encode(event)
		},
		nil,
		nil,
		func(ctx context.Context, err *pbsubstreamsrpc.Error) {
			if err == nil {
				return
			}
			_ = encoder.Encode(errorEvent{
				Kind:    "error",
				Message: fmt.Sprintf("%s: %s", err.Module, err.Reason),
			})
		},
	)

	sinker.Run(context.Background(), sink.NewBlankCursor(), handler)
	return nil
}

func completedRangeCount(progress *pbsubstreamsrpc.ModulesProgress) int {
	total := 0
	for _, stage := range progress.Stages {
		total += len(stage.CompletedRanges)
	}
	return total
}

func highestContiguousBlock(progress *pbsubstreamsrpc.ModulesProgress) uint64 {
	highest := uint64(0)
	for _, stats := range progress.ModulesStats {
		if stats.HighestContiguousBlock > highest {
			highest = stats.HighestContiguousBlock
		}
	}
	return highest
}
