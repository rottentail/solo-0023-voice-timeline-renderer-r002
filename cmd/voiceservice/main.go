package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"voice-timeline-renderer/internal/httpapi"
	"voice-timeline-renderer/internal/service"
	"voice-timeline-renderer/internal/storage"
)

func main() {
	address := flag.String("addr", ":8093", "HTTP listen address")
	dataDir := flag.String("data", "./var/voice-timeline", "persistent data directory")
	maxParallel := flag.Int("max-parallel-renders", 2, "maximum render jobs executing concurrently")
	memoryBudgetMiB := flag.Int("render-memory-mib", 1024, "total render memory budget in mebibytes shared by concurrent renders")
	maxOutputSeconds := flag.Int("max-output-seconds", 1800, "maximum rendered timeline duration in seconds")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0o750); err != nil {
		slog.Error("cannot create data directory", "error", err)
		os.Exit(1)
	}
	repo, err := storage.NewRepository(*dataDir)
	if err != nil {
		slog.Error("cannot initialize repository", "error", err)
		os.Exit(1)
	}
	config := service.DefaultConfig()
	config.MaxParallelRenders = *maxParallel
	config.RenderMemoryBytes = int64(*memoryBudgetMiB) << 20
	config.MaxOutputFrames = int64(48000) * int64(*maxOutputSeconds)
	serviceLayer := service.New(repo, config)

	// Adopt any jobs a previous process left non-terminal.
	recoverCtx, stopRecovery := context.WithTimeout(context.Background(), 60*time.Second)
	serviceLayer.RecoverInterruptedJobs(recoverCtx)
	stopRecovery()

	api := httpapi.New(serviceLayer, httpapi.Options{
		MaxSourceRequestBytes: 48 << 20,
		MaxJSONBytes:          2 << 20,
	})
	httpServer := &http.Server{
		Addr:              *address,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		// Thirty-minute renders must not be cut off by the server deadline.
		WriteTimeout: 40 * time.Minute,
		IdleTimeout:  60 * time.Second,
	}

	runContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-runContext.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownContext)
	}()

	slog.Info("voice timeline service listening",
		"addr", *address,
		"data", *dataDir,
		"max_parallel_renders", config.MaxParallelRenders,
		"render_memory_bytes", config.RenderMemoryBytes,
		"max_output_frames", config.MaxOutputFrames,
	)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("HTTP service stopped", "error", err)
		os.Exit(1)
	}
}
