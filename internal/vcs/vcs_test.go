package vcs

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

const committedFile = "app/main.go"
const committedContent = "package main\n\nfunc main() {}\n"

// wantAuthorization is the header a token has to travel in. The git transport
// answers a Bearer header with 401 and then asks for a username, which a bot
// that disabled the prompt cannot supply, so Basic with the token as the
// password is the only form that works.
func wantAuthorization(token string) string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// localRepo creates a real repository in a temporary directory, with one commit
// on the branch "work", and returns its path and the commit's sha.
func localRepo(t *testing.T) (dir, sha string) {
	t.Helper()
	dir = t.TempDir()

	git(t, dir, "init", "--quiet", "--initial-branch=work", ".")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, committedFile)), 0o755); err != nil {
		t.Fatalf("could not create the directory for %s: %v", committedFile, err)
	}
	if err := os.WriteFile(filepath.Join(dir, committedFile), []byte(committedContent), 0o644); err != nil {
		t.Fatalf("could not write %s: %v", committedFile, err)
	}
	git(t, dir, "add", ".")
	git(t, dir,
		"-c", "user.email=test@example.test",
		"-c", "user.name=Test",
		"commit", "--quiet", "-m", "add "+committedFile,
	)
	return dir, strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
}

// git runs a real git command and fails the test if it does not succeed.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}

// fileURL is the local clone URL of a repository directory.
func fileURL(dir string) string {
	return "file:///" + filepath.ToSlash(dir)
}

// localRemote serves a repository over the dumb HTTP protocol from an
// in-process test server. It is a local remote: nothing leaves the machine, and
// unlike a file:// URL it does not need git to be able to start a shell.
func localRemote(t *testing.T, repo string) string {
	t.Helper()
	// Dumb HTTP clients read info/refs and objects/info/packs.
	git(t, repo, "update-server-info")
	server := httptest.NewServer(http.FileServer(http.Dir(repo)))
	t.Cleanup(server.Close)
	return server.URL + "/.git"
}

func TestFetchChecksOutTheRequestedRevisionFromALocalRepository(t *testing.T) {
	repo, sha := localRepo(t)

	tests := []struct {
		name string
		ref  string
	}{
		{name: "by commit sha", ref: sha},
		{name: "by branch name", ref: "work"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			remote := localRemote(t, repo)

			dir, cleanup, err := Checkout{URL: remote, Ref: test.ref}.Fetch(context.Background())
			if err != nil {
				t.Fatalf("Fetch returned an unexpected error: %v", err)
			}
			defer cleanup()

			if dir == "" {
				t.Fatal("Fetch returned no directory")
			}
			content, err := os.ReadFile(filepath.Join(dir, committedFile))
			if err != nil {
				t.Fatalf("the working tree does not contain %s: %v", committedFile, err)
			}
			// git on Windows checks a committed LF out as CRLF; the line
			// endings are not what this test is about.
			if got := strings.ReplaceAll(string(content), "\r\n", "\n"); got != committedContent {
				t.Errorf("%s = %q, want %q", committedFile, got, committedContent)
			}
			if head := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD")); head != sha {
				t.Errorf("HEAD = %s, want the requested revision %s", head, sha)
			}
			if status := strings.TrimSpace(git(t, dir, "status", "--porcelain")); status != "" {
				t.Errorf("the working tree is dirty after checkout: %q", status)
			}
		})
	}
}

func TestFetchChecksOutARevisionFromAFileURL(t *testing.T) {
	repo, sha := localRepo(t)

	// On Windows the file:// transport runs git-upload-pack through the MSYS
	// shell, which some sandboxes do not allow a process to start. Skip only
	// for that, and only when plain git cannot read the remote either; the
	// checkout path itself is covered by the test above.
	if out, err := lsRemote(fileURL(repo)); err != nil {
		if strings.Contains(out, "signal pipe") {
			t.Skipf("this environment cannot run git's file:// transport: %s", out)
		}
		t.Fatalf("git cannot read the local file:// remote: %v\n%s", err, out)
	}

	dir, cleanup, err := Checkout{URL: fileURL(repo), Ref: sha}.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch returned an unexpected error: %v", err)
	}
	defer cleanup()

	content, err := os.ReadFile(filepath.Join(dir, committedFile))
	if err != nil {
		t.Fatalf("the working tree does not contain %s: %v", committedFile, err)
	}
	// git on Windows checks a committed LF out as CRLF; the line endings are
	// not what this test is about.
	if got := strings.ReplaceAll(string(content), "\r\n", "\n"); got != committedContent {
		t.Errorf("%s = %q, want %q", committedFile, got, committedContent)
	}
	if head := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD")); head != sha {
		t.Errorf("HEAD = %s, want the requested revision %s", head, sha)
	}
}

// lsRemote reports whether this environment can read a remote at all, so that
// the file:// test can tell a broken environment from a broken Fetch.
func lsRemote(url string) (string, error) {
	command := exec.Command("git", "ls-remote", url)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	return string(output), err
}

func TestCleanupRemovesTheDirectoryFetchCreated(t *testing.T) {
	repo, sha := localRepo(t)

	dir, cleanup, err := Checkout{URL: localRemote(t, repo), Ref: sha}.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch returned an unexpected error: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the checkout directory %s is missing: %v", dir, err)
	}

	cleanup()

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%s) after cleanup returned %v, want the directory removed", dir, err)
	}
}

func TestFetchIntoTheCallersOwnDirectoryLeavesItInPlace(t *testing.T) {
	repo, sha := localRepo(t)
	dir := filepath.Join(t.TempDir(), "checkout")

	got, cleanup, err := Checkout{URL: localRemote(t, repo), Ref: sha, Dir: dir}.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch returned an unexpected error: %v", err)
	}
	if got != dir {
		t.Errorf("Fetch returned %q, want the caller's directory %q", got, dir)
	}
	if _, err := os.ReadFile(filepath.Join(dir, committedFile)); err != nil {
		t.Fatalf("the working tree does not contain %s: %v", committedFile, err)
	}

	cleanup()

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the caller's own directory was removed: %v", err)
	}
}

func TestFetchRejectsAnEmptyURLOrAnEmptyRevision(t *testing.T) {
	tests := []struct {
		name    string
		check   Checkout
		wantErr string
	}{
		{name: "no URL", check: Checkout{Ref: "abc123"}, wantErr: "vcs: no repository URL"},
		{name: "no revision", check: Checkout{URL: "file:///nowhere"}, wantErr: "vcs: no revision to check out"},
		{name: "neither", check: Checkout{}, wantErr: "vcs: no repository URL"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir, cleanup, err := test.check.Fetch(context.Background())
			if err == nil {
				t.Fatalf("Fetch returned no error, want %q", test.wantErr)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, test.wantErr)
			}
			if dir != "" {
				t.Errorf("Fetch returned the directory %q, want none", dir)
			}
			if cleanup == nil {
				t.Fatal("Fetch returned no cleanup function")
			}
			cleanup() // must be safe to call even when nothing was created.
		})
	}
}

func TestFetchRemovesTheDirectoryItCreatedWhenTheFetchFails(t *testing.T) {
	var created string
	checkout := Checkout{
		URL: "https://github.com/octo/hello.git",
		Ref: "abc123",
		Exec: func(_ context.Context, dir string, _ []string, args ...string) (string, error) {
			created = dir
			if args[0] == "fetch" {
				return "", fmt.Errorf("vcs: git fetch: repository not found")
			}
			return "", nil
		},
	}

	dir, cleanup, err := checkout.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch returned no error, want the fetch failure")
	}
	if dir != "" {
		t.Errorf("Fetch returned the directory %q, want none", dir)
	}
	if cleanup == nil {
		t.Fatal("Fetch returned no cleanup function")
	}
	if created == "" {
		t.Fatal("the injected runner was never called")
	}
	if _, statErr := os.Stat(created); !os.IsNotExist(statErr) {
		t.Errorf("os.Stat(%s) = %v, want the temporary directory removed after the failure", created, statErr)
	}
	cleanup() // must still be safe.
}

func TestFetchFallsBackToAFullFetchWhenTheShallowFetchFails(t *testing.T) {
	const url = "https://github.com/octo/hello.git"
	dir := t.TempDir()

	var calls [][]string
	checkout := Checkout{
		URL: url,
		Ref: "abc123",
		Dir: dir,
		Exec: func(_ context.Context, _ string, _ []string, args ...string) (string, error) {
			calls = append(calls, args)
			if len(args) > 0 && args[0] == "fetch" && slices.Contains(args, "--depth") {
				// Some servers refuse to fetch a bare commit sha.
				return "", fmt.Errorf("vcs: git fetch: upload-pack: not our ref")
			}
			return "", nil
		},
	}

	got, cleanup, err := checkout.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch returned an unexpected error: %v", err)
	}
	cleanup()

	want := [][]string{
		{"init", "--quiet", "."},
		{"remote", "add", "origin", url},
		{"fetch", "--quiet", "--depth", "1", "origin", "abc123"},
		{"fetch", "--quiet", "origin"},
		{"-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", "FETCH_HEAD"},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Errorf("git calls =\n%q\nwant\n%q", calls, want)
	}
	if got != dir {
		t.Errorf("Fetch returned %q, want %q", got, dir)
	}
}

func TestFetchReportsTheFailureWhenBothFetchesFail(t *testing.T) {
	checkout := Checkout{
		URL: "https://github.com/octo/hello.git",
		Ref: "abc123",
		Dir: t.TempDir(),
		Exec: func(_ context.Context, _ string, _ []string, args ...string) (string, error) {
			if args[0] == "fetch" {
				return "", fmt.Errorf("vcs: git fetch: could not read from remote repository")
			}
			return "", nil
		},
	}

	_, cleanup, err := checkout.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch returned no error, want the fetch failure")
	}
	if !strings.Contains(err.Error(), "could not read from remote repository") {
		t.Errorf("error = %v, want the git failure", err)
	}
	cleanup()
}

func TestFetchFailsWhenTheCheckoutFails(t *testing.T) {
	checkout := Checkout{
		URL: "https://github.com/octo/hello.git",
		Ref: "abc123",
		Dir: t.TempDir(),
		Exec: func(_ context.Context, _ string, _ []string, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "-c" {
				return "", fmt.Errorf("vcs: git -c advice.detachedHead=false checkout: reference is not a tree")
			}
			return "", nil
		},
	}

	_, cleanup, err := checkout.Fetch(context.Background())
	if err == nil {
		t.Fatal("Fetch returned no error, want the checkout failure")
	}
	if !strings.Contains(err.Error(), "reference is not a tree") {
		t.Errorf("error = %v, want the git failure", err)
	}
	cleanup()
}

func TestTheTokenTravelsInTheEnvironmentAndNeverInTheArguments(t *testing.T) {
	// Not a credential: shaped like a token only so that a leak would be
	// unmistakable in a failure message.
	const token = "ghs_not-a-real-token-0000000000000000"

	var args [][]string
	var envs [][]string
	checkout := Checkout{
		URL:   "https://github.com/octo/hello.git",
		Ref:   "abc123",
		Token: token,
		Dir:   t.TempDir(),
		Exec: func(_ context.Context, _ string, env []string, a ...string) (string, error) {
			args = append(args, a)
			envs = append(envs, env)
			return "", nil
		},
	}

	_, cleanup, err := checkout.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch returned an unexpected error: %v", err)
	}
	cleanup()

	if len(args) == 0 {
		t.Fatal("the injected runner was never called")
	}
	for index, call := range args {
		for _, arg := range call {
			if strings.Contains(arg, token) {
				t.Errorf("git call %d has the token on its command line: %q", index, call)
			}
		}
	}
	wantEnv := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=" + wantAuthorization(token),
	}
	for index, env := range envs {
		joined := strings.Join(env, "\n")
		for _, want := range wantEnv {
			if !strings.Contains(joined, want) {
				t.Errorf("git call %d environment is missing %q", index, want)
			}
		}
	}
}

func TestEnvironmentWithoutATokenCarriesNoCredentials(t *testing.T) {
	env := Checkout{}.environment()
	if len(env) < len(os.Environ()) {
		t.Fatalf("environment dropped variables: got %d, want at least %d", len(env), len(os.Environ()))
	}
	added := strings.Join(env[len(os.Environ()):], "\n")
	if !strings.Contains(added, "GIT_TERMINAL_PROMPT=0") {
		t.Error("the environment does not disable the credential prompt")
	}
	if strings.Contains(added, "GIT_CONFIG_COUNT") {
		t.Errorf("the environment carries git configuration without a token: %q", added)
	}
}

func TestEnvironmentCarriesTheConfiguredGitConfiguration(t *testing.T) {
	const token = "ghs_not-a-real-token-0000000000000000"

	tests := []struct {
		name     string
		checkout Checkout
		want     []string
	}{
		{
			name: "the token, then the configured entries in order",
			checkout: Checkout{
				Token:  token,
				Config: []string{"http.sslBackend=openssl", " http.proxy=http://proxy.test:3128"},
			},
			want: []string{
				"GIT_CONFIG_COUNT=3",
				"GIT_CONFIG_KEY_0=http.extraHeader",
				"GIT_CONFIG_VALUE_0=" + wantAuthorization(token),
				"GIT_CONFIG_KEY_1=http.sslBackend",
				"GIT_CONFIG_VALUE_1=openssl",
				"GIT_CONFIG_KEY_2=http.proxy",
				"GIT_CONFIG_VALUE_2=http://proxy.test:3128",
			},
		},
		{
			name:     "configured entries without a token",
			checkout: Checkout{Config: []string{"http.sslBackend=openssl"}},
			want: []string{
				"GIT_CONFIG_COUNT=1",
				"GIT_CONFIG_KEY_0=http.sslBackend",
				"GIT_CONFIG_VALUE_0=openssl",
			},
		},
		{
			name:     "a value that itself contains an equals sign",
			checkout: Checkout{Config: []string{"http.extraHeader=X-Api-Key=abc"}},
			want: []string{
				"GIT_CONFIG_COUNT=1",
				"GIT_CONFIG_KEY_0=http.extraHeader",
				"GIT_CONFIG_VALUE_0=X-Api-Key=abc",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			env := test.checkout.environment()
			if len(env) < len(os.Environ())+1+len(test.want) {
				t.Fatalf("environment has %d entries, want at least %d", len(env), len(os.Environ())+1+len(test.want))
			}
			got := env[len(env)-len(test.want):]
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("git configuration =\n%q\nwant\n%q", got, test.want)
			}
			if first := env[len(os.Environ())]; first != "GIT_TERMINAL_PROMPT=0" {
				t.Errorf("first added variable = %q, want %q", first, "GIT_TERMINAL_PROMPT=0")
			}
		})
	}
}

func TestRedactHidesTheTokenAndLeavesOtherTextAlone(t *testing.T) {
	const token = "ghs_not-a-real-token-0000000000000000"

	tests := []struct {
		name  string
		text  string
		token string
		want  string
	}{
		{
			name:  "a token in a git error is hidden",
			text:  "fatal: could not read from https://x-access-token:" + token + "@github.com/octo/hello.git",
			token: token,
			want:  "fatal: could not read from https://x-access-token:***@github.com/octo/hello.git",
		},
		{
			name:  "every occurrence is hidden",
			text:  token + " then " + token,
			token: token,
			want:  "*** then ***",
		},
		{
			name:  "text without the token is unchanged",
			text:  "fatal: repository 'https://github.com/octo/hello.git/' not found",
			token: token,
			want:  "fatal: repository 'https://github.com/octo/hello.git/' not found",
		},
		{
			name:  "no token means no change",
			text:  "fatal: repository not found",
			token: "",
			want:  "fatal: repository not found",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := redact(test.text, test.token); got != test.want {
				t.Errorf("redact(%q, %q) = %q, want %q", test.text, test.token, got, test.want)
			}
		})
	}
}

func TestRemoteURL(t *testing.T) {
	tests := []struct {
		host string
		repo string
		want string
	}{
		{host: "", repo: "octo/hello", want: "https://github.com/octo/hello.git"},
		{host: "github.example.com", repo: "octo/hello", want: "https://github.example.com/octo/hello.git"},
		{host: "github.example.com", repo: "/octo/hello/", want: "https://github.example.com/octo/hello.git"},
	}

	for _, test := range tests {
		t.Run(test.host+"/"+test.repo, func(t *testing.T) {
			if got := RemoteURL(test.host, test.repo); got != test.want {
				t.Errorf("RemoteURL(%q, %q) = %q, want %q", test.host, test.repo, got, test.want)
			}
		})
	}
}

func TestLocalPath(t *testing.T) {
	tests := []struct {
		repo string
		want string
	}{
		{repo: "octo/hello", want: "octo-hello"},
		{repo: "octo/hello/world", want: "octo-hello-world"},
		{repo: "hello", want: "hello"},
	}

	for _, test := range tests {
		t.Run(test.repo, func(t *testing.T) {
			want := filepath.Join("/tmp/review", test.want)
			if got := LocalPath("/tmp/review", test.repo); got != want {
				t.Errorf("LocalPath(%q, %q) = %q, want %q", "/tmp/review", test.repo, got, want)
			}
		})
	}
}
