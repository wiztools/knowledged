// kc is the command-line client for the knowledged HTTP server.
//
// Usage:
//
//	kc [--server URL] <command> [flags]
//
// Commands:
//
//	post   Store content in the knowledge base
//	get    Retrieve content from the knowledge base
//	job    Check the status of a store job
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const defaultServer = "http://localhost:8080"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	// Global flags (must come before the subcommand).
	serverFlag := flag.String("server", defaultServer, "knowledged server URL")
	flag.Usage = globalUsage
	flag.Parse()

	if flag.NArg() == 0 {
		globalUsage()
		os.Exit(1)
	}

	cmd := flag.Arg(0)
	args := flag.Args()[1:]
	server := strings.TrimRight(*serverFlag, "/")

	switch cmd {
	case "post":
		runPost(server, args, logger)
	case "get":
		runGet(server, args, logger)
	case "job":
		runJob(server, args, logger)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		globalUsage()
		os.Exit(1)
	}
}

// ── post ─────────────────────────────────────────────────────────────────────

func runPost(server string, args []string, logger *slog.Logger) {
	fs := flag.NewFlagSet("post", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: kc post [flags]

Store content in the knowledge base. Content is read from --content,
--file, or stdin (in that priority order).

Flags:`)
		fs.PrintDefaults()
	}

	content := fs.String("content", "", "content to store (inline string)")
	file := fs.String("file", "", "read content from this file path")
	hint := fs.String("hint", "", "optional topic hint for the organizer")
	tags := fs.String("tags", "", "comma-separated tags")
	wait := fs.Bool("wait", false, "poll until the job completes and print the stored path")
	timeout := fs.Int("timeout", 120, "seconds to wait when --wait is set")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}

	body, err := resolveContent(*content, *file)
	if err != nil {
		fatal(logger, "reading content", err)
	}
	if strings.TrimSpace(body) == "" {
		fatal(logger, "reading content", fmt.Errorf("content is empty"))
	}

	var tagList []string
	if *tags != "" {
		for _, t := range strings.Split(*tags, ",") {
			if t = strings.TrimSpace(t); t != "" {
				tagList = append(tagList, t)
			}
		}
	}

	reqBody := map[string]any{
		"content": body,
		"hint":    *hint,
		"tags":    tagList,
	}

	var resp struct {
		JobID  string `json:"job_id"`
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := postJSON(server+"/content", reqBody, &resp); err != nil {
		fatal(logger, "posting content", err)
	}
	if resp.Error != "" {
		fatal(logger, "server error", fmt.Errorf("%s", resp.Error))
	}

	logger.Info("job enqueued", "job_id", resp.JobID, "status", resp.Status)
	fmt.Println(resp.JobID)

	if !*wait {
		return
	}

	logger.Info("waiting for job to complete", "job_id", resp.JobID, "timeout_s", *timeout)
	job, err := pollJob(server, resp.JobID, time.Duration(*timeout)*time.Second, logger)
	if err != nil {
		fatal(logger, "polling job", err)
	}
	printJobResult(job)
}

// ── get ──────────────────────────────────────────────────────────────────────

func runGet(server string, args []string, logger *slog.Logger) {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: kc get [flags]

Retrieve content from the knowledge base.

  --path   returns the raw file content.
  --query  returns a synthesised answer (default) or matching raw docs (--mode raw).

Flags:`)
		fs.PrintDefaults()
	}

	path := fs.String("path", "", "repo-relative file path (e.g. tech/go/basics.md)")
	query := fs.String("query", "", "natural-language query")
	mode := fs.String("mode", "", "response mode: raw | synthesize (default synthesize for --query)")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if *path == "" && *query == "" {
		fs.Usage()
		os.Exit(1)
	}

	params := url.Values{}
	if *path != "" {
		params.Set("path", *path)
	}
	if *query != "" {
		params.Set("query", *query)
	}
	if *mode != "" {
		params.Set("mode", *mode)
	}

	rawBody, err := getRequest(server + "/content?" + params.Encode())
	if err != nil {
		fatal(logger, "GET /content", err)
	}

	// Detect which response shape we got and pretty-print accordingly.
	switch {
	case *path != "" || (*query != "" && *mode == "raw"):
		printRaw(rawBody, logger)
	default:
		printSynthesis(rawBody, logger)
	}
}

// ── job ──────────────────────────────────────────────────────────────────────

func runJob(server string, args []string, logger *slog.Logger) {
	fs := flag.NewFlagSet("job", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: kc job --id <job-id>

Check the status of a store job.

Flags:`)
		fs.PrintDefaults()
	}

	id := fs.String("id", "", "job ID returned by 'kc post'")

	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if *id == "" {
		fs.Usage()
		os.Exit(1)
	}

	var job jobStatus
	if err := getJSON(server+"/jobs/"+*id, &job); err != nil {
		fatal(logger, "GET /jobs", err)
	}
	printJobResult(&job)
}

// ── shared types & helpers ───────────────────────────────────────────────────

type jobStatus struct {
	JobID  string `json:"job_id"`
	Status string `json:"status"`
	Path   string `json:"path"`
	Error  string `json:"error"`
}

// resolveContent picks content from the inline flag, a file, or stdin.
func resolveContent(inline, filePath string) (string, error) {
	if inline != "" {
		return inline, nil
	}
	if filePath != "" {
		data, err := os.ReadFile(filePath)
		if err != nil {
			return "", fmt.Errorf("reading file %s: %w", filePath, err)
		}
		return string(data), nil
	}
	// Fall back to stdin.
	fi, err := os.Stdin.Stat()
	if err != nil {
		return "", fmt.Errorf("stat stdin: %w", err)
	}
	if fi.Mode()&os.ModeCharDevice != 0 {
		// Interactive terminal — prompt the user.
		fmt.Fprintln(os.Stderr, "Enter content (Ctrl-D to finish):")
	}
	var sb strings.Builder
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1 MiB lines
	for scanner.Scan() {
		sb.WriteString(scanner.Text())
		sb.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("reading stdin: %w", err)
	}
	return sb.String(), nil
}

// pollJob repeatedly queries GET /jobs/{id} until the job reaches a terminal
// state or the deadline is exceeded.
func pollJob(server, jobID string, timeout time.Duration, logger *slog.Logger) (*jobStatus, error) {
	deadline := time.Now().Add(timeout)
	interval := 2 * time.Second

	for {
		var job jobStatus
		if err := getJSON(server+"/jobs/"+jobID, &job); err != nil {
			return nil, err
		}
		if job.Status == "done" || job.Status == "failed" {
			return &job, nil
		}
		logger.Info("job still in progress", "status", job.Status)
		if time.Now().Add(interval).After(deadline) {
			return nil, fmt.Errorf("timed out after %s waiting for job %s (last status: %s)",
				timeout, jobID, job.Status)
		}
		time.Sleep(interval)
	}
}

func printJobResult(job *jobStatus) {
	fmt.Printf("job_id : %s\n", job.JobID)
	fmt.Printf("status : %s\n", job.Status)
	if job.Path != "" {
		fmt.Printf("path   : %s\n", job.Path)
	}
	if job.Error != "" {
		fmt.Printf("error  : %s\n", job.Error)
	}
}

// printRaw handles both a single rawDocResponse and a []rawDocResponse.
func printRaw(body []byte, logger *slog.Logger) {
	// Try array first.
	var docs []struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &docs); err == nil {
		for i, d := range docs {
			if i > 0 {
				fmt.Println(strings.Repeat("─", 60))
			}
			fmt.Printf("=== %s ===\n%s\n", d.Path, d.Content)
		}
		return
	}

	// Try single object.
	var doc struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(body, &doc); err == nil {
		if doc.Error != "" {
			fmt.Fprintln(os.Stderr, "error:", doc.Error)
			os.Exit(1)
		}
		fmt.Printf("=== %s ===\n%s\n", doc.Path, doc.Content)
		return
	}

	logger.Warn("unexpected response shape, printing raw JSON")
	fmt.Println(string(body))
}

func printSynthesis(body []byte, logger *slog.Logger) {
	var resp struct {
		Query   string   `json:"query"`
		Sources []string `json:"sources"`
		Answer  string   `json:"answer"`
		Error   string   `json:"error"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		logger.Warn("could not parse synthesis response, printing raw", "error", err)
		fmt.Println(string(body))
		return
	}
	if resp.Error != "" {
		fmt.Fprintln(os.Stderr, "error:", resp.Error)
		os.Exit(1)
	}
	if len(resp.Sources) > 0 {
		fmt.Fprintf(os.Stderr, "sources: %s\n\n", strings.Join(resp.Sources, ", "))
	}
	fmt.Println(resp.Answer)
}

// ── HTTP helpers ─────────────────────────────────────────────────────────────

var httpClient = &http.Client{Timeout: 180 * time.Second}

func postJSON(endpoint string, body any, out any) error {
	var buf strings.Builder
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		return fmt.Errorf("encoding request: %w", err)
	}
	resp, err := httpClient.Post(endpoint, "application/json", strings.NewReader(buf.String()))
	if err != nil {
		return fmt.Errorf("POST %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding response (HTTP %d): %w\nbody: %s", resp.StatusCode, err, string(raw))
	}
	return nil
}

func getJSON(endpoint string, out any) error {
	raw, err := getRequest(endpoint)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decoding response: %w\nbody: %s", err, string(raw))
	}
	return nil
}

func getRequest(endpoint string) ([]byte, error) {
	resp, err := httpClient.Get(endpoint)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}
	return raw, nil
}

// ── usage ────────────────────────────────────────────────────────────────────

func globalUsage() {
	fmt.Fprintln(os.Stderr, `kc — CLI client for knowledged

Usage:
  kc [--server URL] <command> [flags]

Commands:
  post   Store content in the knowledge base
  get    Retrieve content from the knowledge base
  job    Check the status of a store job

Global flags:
  --server string   knowledged server URL (default "http://localhost:8080")

Run 'kc <command> --help' for command-specific flags.

Examples:
  # Store content inline, wait for completion
  kc post --content "Go uses goroutines for concurrency." --hint "golang" --wait

  # Store a file, get back the job ID, check status later
  kc post --file notes.md --tags "meeting,q1"
  kc job --id <job-id>

  # Retrieve a specific file
  kc get --path tech/go/goroutines.md

  # Ask a question (LLM synthesis)
  kc get --query "what do I know about Go concurrency?"

  # Find matching docs without synthesis
  kc get --query "docker" --mode raw`)
}

func fatal(logger *slog.Logger, msg string, err error) {
	logger.Error(msg, "error", err)
	os.Exit(1)
}
