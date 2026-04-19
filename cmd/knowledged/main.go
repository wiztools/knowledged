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
	"github.com/wiztools/knowledged/internal/recentlog"
	"github.com/wiztools/knowledged/internal/store"
)

func main() {
	repoPath := flag.String("repo", "", "path to the knowledge Git repository (required)")
	providerName := flag.String("llm-provider", "ollama", "LLM provider to use (ollama, anthropic, jan)")
	model := flag.String("model", "mistral-small3.1", "LLM model name")
	port := flag.String("port", "9090", "HTTP listen port")
	ollamaURL := flag.String("ollama-url", "http://localhost:11434", "Ollama server base URL")
	janURL := flag.String("jan-url", "http://localhost:8080", "Jan server base URL")
	pushOriginEvery := flag.Duration("push-origin-every", 0, "if greater than zero, periodically push the current branch to origin from the single git worker (for example: 24h)")
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
		"push_origin_every", *pushOriginEvery,
	)

	// ── Store (Git backend) ───────────────────────────────────────────────────
	logger.Info("initializing knowledge store", "path", *repoPath)
	st, err := store.New(*repoPath, logger)
	if err != nil {
		logger.Error("failed to initialize store", "error", err)
		os.Exit(1)
	}
	logger.Info("knowledge store ready", "path", st.RepoPath())

	// ── LLM provider ─────────────────────────────────────────────────────────
	var provider llm.Provider
	switch *providerName {
	case "ollama":
		provider = llm.NewOllama(*ollamaURL, *model, logger)
		logger.Info("LLM provider initialized",
			"provider", "ollama",
			"url", *ollamaURL,
			"model", *model)
	case "anthropic":
		apiKey := os.Getenv("ANTHROPIC_API_KEY")
		if apiKey == "" {
			logger.Error("ANTHROPIC_API_KEY environment variable is not set")
			os.Exit(1)
		}
		// Default to claude-3-5-haiku if the user didn't override the model flag.
		if *model == "mistral-small3.1" {
			*model = "claude-sonnet-4-6"
		}
		provider = llm.NewAnthropic(apiKey, *model, logger)
		logger.Info("LLM provider initialized",
			"provider", "anthropic",
			"model", *model)
	case "jan":
		if *model == "mistral-small3.1" {
			*model = "Jan-v3.5-4B-Q4_K_XL"
		}
		provider = llm.NewJan(*janURL, *model, logger)
		logger.Info("LLM provider initialized",
			"provider", "jan",
			"url", *janURL,
			"model", *model)
	default:
		logger.Error("unknown LLM provider", "provider", *providerName,
			"supported", []string{"ollama", "anthropic", "jan"})
		os.Exit(1)
	}

	// ── Organizer ─────────────────────────────────────────────────────────────
	org := organizer.New(st, provider, logger)
	logger.Info("organizer initialized")

	// ── Recent-posts log ──────────────────────────────────────────────────────
	rl := recentlog.New(st.StatePath("recent-posts.jsonl"), logger)
	logger.Info("recent-posts log initialized", "path", st.StatePath("recent-posts.jsonl"))

	// ── Queue ─────────────────────────────────────────────────────────────────
	logger.Info("initializing job queue")
	q, err := queue.New(st, org, rl, logger, *pushOriginEvery)
	if err != nil {
		logger.Error("failed to initialize queue", "error", err)
		os.Exit(1)
	}
	logger.Info("job queue ready")

	// ── Context + queue worker ────────────────────────────────────────────────
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Start(ctx)

	// ── HTTP server ───────────────────────────────────────────────────────────
	h := api.NewHandler(q, st, provider, rl, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /content", h.PostContent)
	mux.HandleFunc("DELETE /content", h.DeleteContent)
	mux.HandleFunc("GET /content", h.GetContent)
	mux.HandleFunc("GET /jobs/{id}", h.GetJob)
	mux.HandleFunc("GET /posts/recents", h.GetRecentPosts)

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
