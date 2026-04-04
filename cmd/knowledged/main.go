package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wiztools/knowledged/internal/api"
	"github.com/wiztools/knowledged/internal/llm"
	"github.com/wiztools/knowledged/internal/organizer"
	"github.com/wiztools/knowledged/internal/queue"
	"github.com/wiztools/knowledged/internal/store"
)

func main() {
	repoPath := flag.String("repo", "", "path to the knowledge Git repository (required)")
	providerName := flag.String("llm-provider", "ollama", "LLM provider to use (currently: ollama)")
	model := flag.String("model", "mistral-small3.1", "LLM model name")
	port := flag.String("port", "8080", "HTTP listen port")
	ollamaURL := flag.String("ollama-url", "http://localhost:11434", "Ollama server base URL")
	flag.Parse()

	// ── Logger ────────────────────────────────────────────────────────────────
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))
	slog.SetDefault(logger)

	// ── Validate required flags ───────────────────────────────────────────────
	if *repoPath == "" {
		logger.Error("--repo is required")
		flag.Usage()
		os.Exit(1)
	}

	logger.Info("starting knowledged",
		"repo", *repoPath,
		"llm_provider", *providerName,
		"model", *model,
		"port", *port,
	)

	// ── Store (Git backend) ───────────────────────────────────────────────────
	logger.Info("initialising knowledge store", "path", *repoPath)
	st, err := store.New(*repoPath, logger)
	if err != nil {
		logger.Error("failed to initialise store", "error", err)
		os.Exit(1)
	}
	logger.Info("knowledge store ready", "path", st.RepoPath())

	// ── LLM provider ─────────────────────────────────────────────────────────
	var provider llm.Provider
	switch *providerName {
	case "ollama":
		provider = llm.NewOllama(*ollamaURL, *model, logger)
		logger.Info("LLM provider initialised",
			"provider", "ollama",
			"url", *ollamaURL,
			"model", *model)
	default:
		logger.Error("unknown LLM provider", "provider", *providerName,
			"supported", []string{"ollama"})
		os.Exit(1)
	}

	// ── Organizer ─────────────────────────────────────────────────────────────
	org := organizer.New(st, provider, logger)
	logger.Info("organizer initialised")

	// ── Queue ─────────────────────────────────────────────────────────────────
	logger.Info("initialising job queue")
	q, err := queue.New(st, org, logger)
	if err != nil {
		logger.Error("failed to initialise queue", "error", err)
		os.Exit(1)
	}
	logger.Info("job queue ready")

	// ── Context + queue worker ────────────────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Start(ctx)

	// ── HTTP server ───────────────────────────────────────────────────────────
	h := api.NewHandler(q, st, provider, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /content", h.PostContent)
	mux.HandleFunc("GET /content", h.GetContent)
	mux.HandleFunc("GET /jobs/{id}", h.GetJob)

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%s", *port),
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 180 * time.Second, // synthesis calls can be slow
		IdleTimeout:  60 * time.Second,
	}

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logger.Info("HTTP server listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server error", "error", err)
			os.Exit(1)
		}
	}()

	sig := <-stop
	logger.Info("received shutdown signal", "signal", sig)
	cancel() // stop the queue worker

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server shutdown error", "error", err)
	}
	logger.Info("knowledged stopped cleanly")
}
