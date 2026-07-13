// Command reindex regenerates INDEX.md as a deterministic projection of the
// notes in a knowledged repository and commits the result.
//
// INDEX.md is normally rebuilt on every store/edit/delete by the server. This
// standalone tool is for maintenance: repairing an index that predates the
// rebuild-on-write behavior, or forcing a regeneration. Run it while the
// server is stopped — it writes and commits directly and would otherwise race
// the single git worker.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/wiztools/knowledged/internal/store"
)

func main() {
	repoPath := flag.String("repo", "", "path to the knowledge Git repository (required)")
	dryRun := flag.Bool("dry-run", false, "print the regenerated INDEX.md to stdout without writing or committing")
	flag.Parse()

	if *repoPath == "" {
		fmt.Fprintln(os.Stderr, "reindex: -repo is required")
		flag.Usage()
		os.Exit(2)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	st, err := store.New(*repoPath, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reindex: opening store: %v\n", err)
		os.Exit(1)
	}

	if *dryRun {
		notes, err := st.ListMarkdownNotes()
		if err != nil {
			fmt.Fprintf(os.Stderr, "reindex: listing notes: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(store.RebuildIndex(notes))
		return
	}

	if err := st.RebuildAndWriteIndex(); err != nil {
		fmt.Fprintf(os.Stderr, "reindex: rebuilding index: %v\n", err)
		os.Exit(1)
	}
	if err := st.Commit("reindex: regenerate INDEX.md from notes"); err != nil {
		fmt.Fprintf(os.Stderr, "reindex: committing: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("reindex: INDEX.md regenerated and committed")
}
