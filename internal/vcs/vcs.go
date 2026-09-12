// Package vcs checks out the code a pull request changes, so that a linter can
// run over whole files rather than only the diff.
package vcs

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Checkout describes a repository revision to fetch.
type Checkout struct {
	// URL is the clone URL, for example https://github.com/owner/name.git.
	URL string

	// Ref is the commit or branch to check out.
	Ref string

	// Dir is where to put it. Empty means a new temporary directory, which is
	// removed when the caller is finished.
	Dir string

	// Depth limits the history fetched. One is enough to review a commit and
	// is much faster than the whole repository.
	Depth int

	// Token authenticates a private repository. It is passed through the
	// environment rather than the command line, where every process on the
	// machine could read it.
	Token string

	// Config is extra git configuration for the fetch, as "key=value" entries.
	// A deployment needs it for a proxy, a private certificate authority, or a
	// TLS backend: a machine whose default backend cannot complete the
	// handshake is fixed with "http.sslBackend=openssl".
	Config []string

	// Timeout bounds the fetch.
	Timeout time.Duration

	// Exec runs git. It exists so a test can watch the commands without a
	// remote, and so a deployment can run the fetch somewhere else.
	Exec func(ctx context.Context, dir string, env []string, args ...string) (string, error)
}

// Fetch checks the revision out and returns the directory, plus a function that
// removes it if this call created it.
func (c Checkout) Fetch(ctx context.Context) (string, func(), error) {
	if c.URL == "" {
		return "", func() {}, fmt.Errorf("vcs: no repository URL")
	}
	if c.Ref == "" {
		return "", func() {}, fmt.Errorf("vcs: no revision to check out")
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}

	dir := c.Dir
	cleanup := func() {}
	if dir == "" {
		created, err := os.MkdirTemp("", "review-checkout-")
		if err != nil {
			return "", func() {}, fmt.Errorf("vcs: could not create a working directory: %w", err)
		}
		dir = created
		cleanup = func() { _ = os.RemoveAll(created) }
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", func() {}, fmt.Errorf("vcs: could not create %s: %w", dir, err)
	}

	env := c.environment()
	fail := func(err error) (string, func(), error) {
		cleanup()
		return "", func() {}, err
	}

	// A shallow fetch of one revision is much faster than a clone, and it is
	// all a review of a commit needs.
	steps := [][]string{
		{"init", "--quiet", "."},
		{"remote", "add", "origin", c.URL},
	}
	for _, args := range steps {
		if _, err := c.exec(ctx, dir, env, args...); err != nil {
			return fail(err)
		}
	}

	depth := c.Depth
	if depth <= 0 {
		depth = 1
	}
	fetch := []string{"fetch", "--quiet", "--depth", fmt.Sprint(depth), "origin", c.Ref}
	if _, err := c.exec(ctx, dir, env, fetch...); err != nil {
		// Some servers refuse to fetch a bare commit sha. Fetching everything
		// is slower but works, and a review that fails on a valid commit is
		// worse than a slow one.
		if _, retryErr := c.exec(ctx, dir, env, "fetch", "--quiet", "origin"); retryErr != nil {
			return fail(err)
		}
	}
	if _, err := c.exec(ctx, dir, env, "-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", "FETCH_HEAD"); err != nil {
		return fail(err)
	}
	return dir, cleanup, nil
}

// environment is the environment git runs in. The token, when there is one, is
// passed as configuration through the environment: putting it in the command
// line would show it to every process on the machine.
func (c Checkout) environment() []string {
	env := os.Environ()
	// A bot must fail rather than block for ever on a credential prompt.
	env = append(env, "GIT_TERMINAL_PROMPT=0")

	entries := make([]string, 0, len(c.Config)+1)
	if c.Token != "" {
		entries = append(entries, "http.extraHeader=Authorization: Basic "+basicAuth(c.Token))
	}
	entries = append(entries, c.Config...)

	if len(entries) == 0 {
		return env
	}
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(entries)))
	for index, entry := range entries {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			// A malformed entry is skipped rather than passed on, because git
			// would read the whole run's configuration as broken.
			continue
		}
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", index, strings.TrimSpace(key)),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", index, value),
		)
	}
	return env
}

func (c Checkout) exec(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	if c.Exec != nil {
		return c.Exec(ctx, dir, env, args...)
	}
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.Env = env
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return stdout.String(), fmt.Errorf("vcs: git %s: %s", strings.Join(args, " "), redact(message, c.Token))
	}
	return stdout.String(), nil
}

// basicAuth encodes a token the way the git transport expects it.
//
// The REST API takes "Authorization: Bearer <token>", but the git smart HTTP
// endpoint does not: a Bearer header is answered with 401, git then asks for a
// username, and a bot that promised not to prompt fails with "could not read
// Username". Basic authentication with the token as the password is what works,
// and "x-access-token" is the user GitHub documents for it.
func basicAuth(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
}

// redact removes a token from anything that might be logged or returned.
func redact(text, token string) string {
	if token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "***")
}

// RemoteURL builds the clone URL for a repository.
func RemoteURL(host, repo string) string {
	if host == "" {
		host = "github.com"
	}
	return fmt.Sprintf("https://%s/%s.git", host, strings.Trim(repo, "/"))
}

// LocalPath turns a repository name into a path below dir.
func LocalPath(dir, repo string) string {
	return filepath.Join(dir, strings.ReplaceAll(repo, "/", "-"))
}
