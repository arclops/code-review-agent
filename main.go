// Command code-review-agent reviews GitHub pull requests and posts one comment
// back.
//
// It has three modes. "review" reviews one pull request from the terminal,
// which is the fastest way to see what it would say. "serve" runs the webhook
// server that GitHub calls on every push. "runs" reads the execution history
// from a running server, so a failure nobody noticed in a log is still visible
// afterwards.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/arclops/code-review-agent/internal/config"
	"github.com/arclops/code-review-agent/internal/github"
	"github.com/arclops/code-review-agent/internal/model"
	"github.com/arclops/code-review-agent/internal/notify"
	"github.com/arclops/code-review-agent/internal/review"
	"github.com/arclops/code-review-agent/internal/runner"
	"github.com/arclops/code-review-agent/internal/vcs"
	"github.com/arclops/code-review-agent/internal/webhook"
)

// version is the released version of this build.
const version = "1.0.0"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}

	switch args[0] {
	case "serve":
		return serve(args[1:], stdout, stderr)
	case "review":
		return reviewOnce(args[1:], stdout, stderr)
	case "runs":
		return listRuns(args[1:], stdout, stderr)
	case "version", "-version", "--version":
		fmt.Fprintf(stdout, "code-review-agent %s\n", version)
		return 0
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n", args[0])
		usage(stderr)
		return 2
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `code-review-agent reviews GitHub pull requests with static analysis and a
language model, and posts a single comment.

  code-review-agent serve  [-addr :8080] [-config review.json]
      Run the webhook server. GitHub calls POST /webhook/github and the review
      runs in the background. Reads the webhook secret from
      GITHUB_WEBHOOK_SECRET and the API token from GITHUB_TOKEN.

  code-review-agent review -repo owner/name -pr 42 [-post] [-config review.json]
      Review one pull request now. Without -post the review is printed and
      nothing is sent to GitHub.

  code-review-agent runs   [-server http://127.0.0.1:8080]
      Print the run history of a running server, newest first.

  code-review-agent version

Configuration comes from -config, a JSON file. Anything omitted keeps its
default. The environment variables are GITHUB_TOKEN, GITHUB_WEBHOOK_SECRET,
REVIEW_ADDR and REVIEW_NOTIFY_URL.
`)
}

// deps is everything a review needs to touch the outside world.
type deps struct {
	github   *github.Client
	provider model.Provider
	checkout review.CheckoutFn
	notifier *notify.Notifier
}

// loadConfig reads the configuration file and then the environment.
func loadConfig(path string) (config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return config.Config{}, err
	}
	cfg.ApplyEnv(os.Getenv)
	if err := cfg.Validate(); err != nil {
		return config.Config{}, err
	}
	return cfg, nil
}

// buildDeps builds the clients. The token is required: without it the service
// cannot read a pull request, and a reviewer that silently reviews nothing is
// worse than one that refuses to start.
func buildDeps(cfg config.Config) (deps, error) {
	if cfg.GithubToken == "" {
		return deps{}, fmt.Errorf("no GitHub token: set %s", cfg.TokenEnv)
	}

	provider, err := model.New(cfg, os.Getenv)
	if err != nil {
		return deps{}, err
	}

	var checkout review.CheckoutFn
	if cfg.Checkout.Enabled {
		checkout = func(ctx context.Context, url, ref string) (string, func(), error) {
			return vcs.Checkout{
				URL:     url,
				Ref:     ref,
				Depth:   cfg.Checkout.Depth,
				Token:   cfg.GithubToken,
				Config:  cfg.Checkout.GitConfig,
				Timeout: cfg.Limits.RunTimeout.Duration(),
			}.Fetch(ctx)
		}
	}

	return deps{
		github:   github.New(cfg.APIURL, cfg.GithubToken),
		provider: provider,
		checkout: checkout,
		notifier: notify.New(os.Getenv(cfg.Notify.URLEnv), cfg.Notify.Timeout.Duration()),
	}, nil
}

func reviewOptions(cfg config.Config, built deps, logger *log.Logger) review.Options {
	return review.Options{
		Config:   cfg,
		GitHub:   built.github,
		Model:    built.provider,
		Checkout: built.checkout,
		Logger:   logger,
	}
}

// reviewerFor turns one queued job into one posted review.
func reviewerFor(cfg config.Config, built deps, logger *log.Logger) runner.Reviewer {
	return func(ctx context.Context, job runner.Job) (runner.Outcome, error) {
		options := reviewOptions(cfg, built, logger)

		result, err := review.Run(ctx, job.Repository, job.Number, options)
		outcome := runner.Outcome{Findings: len(result.Findings)}
		if err != nil {
			return outcome, err
		}

		// A run that was superseded has nothing useful to say to anybody, and
		// posting it would put a stale review on a pull request that has moved.
		if ctx.Err() != nil {
			return outcome, ctx.Err()
		}

		if err := review.Post(ctx, &result, options); err != nil {
			return outcome, err
		}

		outcome.Posted = result.Posted
		outcome.Comment = result.CommentID
		outcome.Skipped = !result.Posted
		outcome.Reason = result.Reason
		return outcome, nil
	}
}

func serve(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	addr := flags.String("addr", envOr("REVIEW_ADDR", ":8080"), "address to listen on")
	configPath := flags.String("config", "", "path to a JSON configuration file")
	secretEnv := flags.String("webhook-secret-env", "GITHUB_WEBHOOK_SECRET", "environment variable holding the webhook secret")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	logger := log.New(stderr, "", log.LstdFlags|log.LUTC)
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return fail(stderr, err)
	}
	built, err := buildDeps(cfg)
	if err != nil {
		return fail(stderr, err)
	}

	secret := os.Getenv(*secretEnv)
	if secret == "" {
		// Refusing every delivery is the safe default, but it looks like a
		// broken deployment unless the reason is said out loud.
		logger.Printf("WARNING: %s is empty, so every webhook delivery will be refused", *secretEnv)
	}

	queue := runner.New(runner.Options{
		Reviewer:      reviewerFor(cfg, built, logger),
		MaxConcurrent: cfg.Limits.MaxConcurrentRuns,
		Logger:        logger,
		OnFailure: func(record runner.Record) {
			if !built.notifier.Enabled() {
				return
			}
			message := fmt.Sprintf("code-review-agent run %s failed for %s#%d: %s",
				record.ID, record.Repository, record.Number, record.Error)
			ctx, cancel := context.WithTimeout(context.Background(), cfg.Notify.Timeout.Duration())
			defer cancel()
			if err := built.notifier.Notify(ctx, message); err != nil {
				logger.Printf("could not send the failure notification: %v", err)
			}
		},
	})

	handler := webhook.New(webhook.Options{Secret: secret, Queue: queue, Logger: logger})
	server := &http.Server{
		Addr:              *addr,
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger.Printf("code-review-agent %s listening on %s (model %s, %d guidelines)",
		version, *addr, built.provider.Name(), len(cfg.Guidelines))

	served := make(chan error, 1)
	go func() { served <- server.ListenAndServe() }()

	select {
	case err := <-served:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fail(stderr, fmt.Errorf("the server stopped: %w", err))
		}
	case <-ctx.Done():
		logger.Printf("shutting down")
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			logger.Printf("the shutdown was not clean: %v", err)
		}
	}

	// Reviews already running are allowed to finish: half a review on a pull
	// request is worse than a slow shutdown.
	queue.Wait()
	return 0
}

func reviewOnce(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("review", flag.ContinueOnError)
	flags.SetOutput(stderr)
	repo := flags.String("repo", "", "repository as owner/name")
	number := flags.Int("pr", 0, "pull request number")
	post := flags.Bool("post", false, "post the review to GitHub instead of printing it")
	configPath := flags.String("config", "", "path to a JSON configuration file")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	if _, _, err := github.ParseRepo(*repo); err != nil {
		fmt.Fprintf(stderr, "-repo must be owner/name: %v\n", err)
		return 2
	}
	if *number <= 0 {
		fmt.Fprintln(stderr, "-pr must be a pull request number")
		return 2
	}

	logger := log.New(stderr, "", log.LstdFlags|log.LUTC)
	cfg, err := loadConfig(*configPath)
	if err != nil {
		return fail(stderr, err)
	}
	built, err := buildDeps(cfg)
	if err != nil {
		return fail(stderr, err)
	}
	options := reviewOptions(cfg, built, logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, cfg.Limits.RunTimeout.Duration())
	defer cancel()

	result, err := review.Run(ctx, *repo, *number, options)
	if err != nil {
		return fail(stderr, err)
	}
	if *post {
		if err := review.Post(ctx, &result, options); err != nil {
			return fail(stderr, err)
		}
	}

	printResult(stdout, result, *post)
	return 0
}

// printResult writes the review in a form that reads well in a terminal, with
// the comment itself last so that it can be piped somewhere.
func printResult(w io.Writer, result review.Result, posted bool) {
	fmt.Fprintf(w, "%s#%d at %s\n", result.Repository, result.Number, shortSHA(result.HeadSHA))
	fmt.Fprintf(w, "%d file(s) reviewed, %d finding(s), %d guideline(s)\n",
		result.FilesReviewed, len(result.Findings), result.Guidelines)

	for _, skipped := range result.FilesSkipped {
		fmt.Fprintf(w, "  skipped %s: %s\n", skipped.File, skipped.Reason)
	}
	for _, finding := range result.Findings {
		fmt.Fprintf(w, "  [%s] %s:%d %s (%s)\n", finding.Severity, finding.File, finding.Line, finding.Title, finding.Source)
	}
	for _, problem := range result.ModelErrors {
		fmt.Fprintf(w, "  model error: %s\n", problem)
	}
	for _, usage := range result.Usage {
		fmt.Fprintf(w, "  %s: %d prompt + %d completion tokens\n", usage.Model, usage.PromptTokens, usage.CompletionTokens)
	}

	switch {
	case posted && result.Posted:
		verb := "posted"
		if result.Updated {
			verb = "updated"
		}
		fmt.Fprintf(w, "%s comment %d on %s#%d\n", verb, result.CommentID, result.Repository, result.Number)
	case result.Reason != "":
		fmt.Fprintf(w, "nothing sent to GitHub: %s\n", result.Reason)
	default:
		fmt.Fprintln(w, "nothing sent to GitHub: -post was not given")
	}

	if result.Comment != "" {
		fmt.Fprintln(w)
		fmt.Fprintln(w, result.Comment)
	}
}

func listRuns(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("runs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	server := flags.String("server", envOr("REVIEW_SERVER", "http://127.0.0.1:8080"), "address of a running server")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	url := strings.TrimSuffix(*server, "/") + "/runs"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fail(stderr, err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fail(stderr, fmt.Errorf("could not reach %s: %w", url, err))
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fail(stderr, fmt.Errorf("%s answered %s", url, response.Status))
	}

	var payload struct {
		Runs []runner.Record `json:"runs"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&payload); err != nil {
		return fail(stderr, fmt.Errorf("%s did not answer with a run list: %w", url, err))
	}

	if len(payload.Runs) == 0 {
		fmt.Fprintln(stdout, "no runs yet")
		return 0
	}

	fmt.Fprintf(stdout, "%-14s %-10s %-28s %-8s %-8s %s\n", "ID", "STATE", "PULL REQUEST", "FINDINGS", "POSTED", "REASON")
	for _, record := range payload.Runs {
		reason := record.Reason
		if record.Error != "" {
			reason = record.Error
		}
		fmt.Fprintf(stdout, "%-14s %-10s %-28s %-8d %-8t %s\n",
			record.ID, record.State, fmt.Sprintf("%s#%d", record.Repository, record.Number),
			record.Findings, record.Posted, truncate(reason, 80))
	}
	return 0
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func fail(w io.Writer, err error) int {
	fmt.Fprintf(w, "code-review-agent: %v\n", err)
	return 1
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit-3] + "..."
}
