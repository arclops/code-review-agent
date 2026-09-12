# code-review-agent

[![CI](https://github.com/arclops/code-review-agent/actions/workflows/ci.yml/badge.svg)](https://github.com/arclops/code-review-agent/actions/workflows/ci.yml)
![Go](https://img.shields.io/badge/go-1.22%2B-00ADD8)
![License](https://img.shields.io/badge/license-MIT-blue)
![Dependencies](https://img.shields.io/badge/dependencies-none-brightgreen)

A GitHub bot that reviews pull requests: it runs the real linters over the
changed code, asks a language model about what the linters cannot see, merges
both into one comment, and puts that comment on the pull request.

It is a GitHub webhook service and a command line tool in one binary, written in
Go with **no dependencies outside the standard library**.

```
$ code-review-agent review -repo acme/widget -pr 1 -config review.json -post
acme/widget#1 at 9d147a7
1 file(s) reviewed, 3 finding(s), 6 guideline(s)
  [CRITICAL] internal/config/loader.go:5 hardcoded-credential (secret-scan)
  [MAJOR] internal/config/loader.go:9 vet-printf (go-vet)
  [MINOR] internal/config/loader.go:0 gofmt (gofmt)
updated comment 5645778411 on acme/widget#1
```

That run is real: it is the output of this program against
[arclops/pr-review-demo#1](https://github.com/arclops/pr-review-demo/pull/1),
and the comment it names is
[on the pull request](https://github.com/arclops/pr-review-demo/pull/1#issuecomment-5645778411).
The repository has one comment on it, not one per run: the second run edited the
first comment instead of adding another.

Built from a project brief for an automated GitHub pull request review agent: the
deterministic tools and the model each do the part they are good at, and the
result is one comment that is rewritten rather than repeated.

## What makes it different from a prompt wrapper

Four decisions do most of the work, and each of them exists because the
alternative is a bot people mute:

**The tools run first, and their findings are facts.** `go vet` knows the
`Printf` argument is the wrong type; a model asked to spot that is doing an
expensive impression of a compiler. Linter findings are collected before the
model is called and handed to it as facts it must not repeat — and if the model
repeats one anyway, the duplicate is dropped and the tool's precise version
wins.

**A pull request gets one comment, ever.** The bot's comment carries a marker,
and every later run finds it and rewrites it. Three pushes produce one comment
that describes the third push, not three comments describing three dead ones.
The live pull request above shows this: comment `5645778411` was created at
12:06:58 and updated at 12:08:27.

**Silence when there is nothing to say.** A clean pull request gets no comment
at all, because "looks good to me" on every trivial change is what teaches
people to stop reading. If an earlier run posted findings that are now fixed,
the old comment is rewritten to say so, because stale findings are worse than
none.

**A broken review must not look like a clean one.** If the model fails for every
file, the run is recorded as failed and reported to the operator rather than
posting "no findings". A review nobody could run and a review that found nothing
are different things, and only one of them is good news.

## How a review runs

```
GitHub webhook ──▶ signature check ──▶ event filter ──▶ run queue (one per PR)
                                                            │
                            ┌───────────────────────────────┘
                            ▼
                    read the pull request and its files
                            │
              ┌─────────────┴──────────────┐
              ▼                            ▼
     secret scan of the added lines   git checkout of the head commit
     (no clone needed)                       │
              │                              ▼
              │                     configured linters, in parallel
              │                     (go vet, gofmt, ruff, eslint, …)
              └─────────────┬────────────────┘
                            ▼
              the model, one file at a time, bounded concurrency,
              given the linter findings as facts
                            │
                            ▼
        merge · drop duplicates · sort worst first · render
                            │
                            ▼
        post once, or update the comment this bot already owns
```

Nothing about the pipeline is a straight line to a model: the deterministic
results are worth posting on their own, and they are what the live run above
posted, because the demo configured no model.

## Quick start

```bash
go install github.com/arclops/code-review-agent@latest
```

Review one pull request and print what it would say, without posting:

```bash
export GITHUB_TOKEN=ghp_…                  # needs repo scope to comment
code-review-agent review -repo owner/name -pr 42
```

Add `-post` to put the comment on the pull request.

Run it as a service, with the webhook secret in the environment:

```bash
export GITHUB_TOKEN=ghp_…
export GITHUB_WEBHOOK_SECRET=the-secret-you-typed-into-GitHub
code-review-agent serve -addr :8080 -config review.json
```

Point a repository webhook at `https://your-host/webhook/github`, content type
`application/json`, with that same secret, and subscribe to pull request events.
`opened`, `reopened` and `synchronize` start a review; every other action and
event is acknowledged and ignored, because a bot that reviews a pull request
somebody just added a label to is a bot that gets turned off.

The service exposes its own history, so a failure nobody saw in a log is still
findable. This is the real output of a server that reviewed the demo pull
request over a signed webhook delivery, and of a second one pointed at an
unreachable API so that a genuine failure would be recorded:

```
$ code-review-agent runs -server http://127.0.0.1:8099
ID             STATE      PULL REQUEST                 FINDINGS POSTED   REASON
c61b57b2       succeeded  arclops/pr-review-demo#1     3        true

$ code-review-agent runs -server http://127.0.0.1:8098
ID             STATE      PULL REQUEST                 FINDINGS POSTED   REASON
caa4d856       failed     arclops/pr-review-demo#1     0        false    could not read pull request arclops/pr-review-demo#1: gave up after 3 attempt...
```

A failed run also notifies, once, with the run id in the message — sent to a
real HTTP sink during that same demonstration, which received both keys, so one
setting works for Slack and Discord:

```json
{"username":"code-review-agent",
 "text":"code-review-agent run caa4d856 failed for arclops/pr-review-demo#1: …",
 "content":"code-review-agent run caa4d856 failed for arclops/pr-review-demo#1: …"}
```

## Configuration

Everything has a working default, so the service runs with nothing but a token.
`examples/review.json` is a complete configuration, and it is loaded and
validated by the test suite so that it cannot rot.

| Key | What it does |
| --- | --- |
| `guidelines` | The rules the model is asked to review against. They end up in the prompt, and the count is shown in the comment. |
| `model.provider` | `openai` (anything speaking the chat completions API: OpenAI, DeepSeek, Groq, Ollama, …), `anthropic`, or `none` for the deterministic checks alone. |
| `model.base_url`, `model.name`, `model.api_key_env` | Where to call, which model, and which environment variable holds the key. Switching provider is a base URL and a model name. |
| `lint.commands[]` | The tools to run: `command`, `format` (`go-vet`, `gofmt-list`, `ruff-json`, `eslint-json`, `plain`), and `stream` when the tool reports on stderr — which is where the Go toolchain reports, so a runner that only read stdout would call `go vet` clean. |
| `lint.secret_scan` | The built-in scan for committed credentials. On by default. |
| `lint.severity` | Maps a rule to `CRITICAL`, `MAJOR` or `MINOR`, by longest matching prefix. |
| `filters` | Skip patterns (lock files, vendored code, generated files), size limits, and whether to review deleted files. |
| `limits` | Concurrent runs, concurrent per-file model calls, timeouts, and retry counts. |
| `checkout` | Whether to clone the head commit so the linters can see whole files, plus `git_config` for a proxy, a private CA, or a TLS backend. |
| `notify.url_env` | A Slack or Discord webhook for failed runs. One setting works for either: the payload carries both `text` and `content`. |
| `api_url` | A different GitHub API endpoint, for GitHub Enterprise Server. `GITHUB_API_URL` overrides it, which is the variable GitHub Actions already sets. |

Environment variables: `GITHUB_TOKEN`, `GITHUB_WEBHOOK_SECRET`, `REVIEW_ADDR`,
`REVIEW_NOTIFY_URL`, `GITHUB_API_URL`.

## What was verified, and what was not

This section is the honest part.

**Verified against the real GitHub API.** The run at the top of this file is a
real review of a real pull request, posted by this program. It exercised: the
REST calls that read the pull request and its files, the shallow `git fetch` of
the head commit over HTTPS with a token, `go vet` and `gofmt` executed over that
checkout, the built-in secret scan over the patch, the rules that filter linter
output to changed lines, the comment rendering, and the update-in-place
behaviour — the pull request carries one comment that was created and then
updated rather than two comments.

**Verified against a fake endpoint.** The model path is tested end to end in
this repository's test suite: an `httptest` server stands in for the provider,
the service is driven through a signed webhook delivery, and the tests assert
the request shape (path, auth header, `response_format`, the prompt carrying the
lint findings) and that the model's findings reach the posted comment. The
failure path is covered the same way: a model that answers 500 for every file
produces a failed run and a failure notification, and posts no comment. **A real
commercial model was not called**, because no API key was available in the
environment this was built in; the demo pull request therefore runs with
`"provider": "none"`.

**Verified in CI.** The workflow runs the suite on Linux, macOS and Windows,
plus the race detector on Linux. Two tests run the real `go` and `gofmt`
binaries over a checked-out fixture, so the linter integration is exercised on
every platform, not mocked.

**Not verified.** No GitHub App was registered: the service runs as a user token
behind a repository webhook, which is the simpler of the two and the one the
challenge describes. The webhook endpoint has not been driven by GitHub's own
delivery mechanism in production — deliveries are constructed and signed in the
tests, and the signature check is implemented against GitHub's documented
`X-Hub-Signature-256` scheme.

## Tests

```
$ go test ./... -count=1
ok  github.com/arclops/code-review-agent
ok  github.com/arclops/code-review-agent/internal/config
ok  github.com/arclops/code-review-agent/internal/diff
ok  github.com/arclops/code-review-agent/internal/github
ok  github.com/arclops/code-review-agent/internal/httpx
ok  github.com/arclops/code-review-agent/internal/lint
ok  github.com/arclops/code-review-agent/internal/model
ok  github.com/arclops/code-review-agent/internal/notify
ok  github.com/arclops/code-review-agent/internal/review
ok  github.com/arclops/code-review-agent/internal/runner
ok  github.com/arclops/code-review-agent/internal/vcs
ok  github.com/arclops/code-review-agent/internal/webhook
```

204 test functions, and about 7,100 lines of test code against 3,500 lines of
source. They are hermetic — a fake GitHub, a fake model, a local git repository
— with three deliberate exceptions that run something real: `git` against a
repository in a temporary directory, and the `go` and `gofmt` binaries over a
fixture, because a test double for a linter proves nothing about a linter.

The end-to-end test in `main_test.go` is the one worth reading: it signs a
webhook delivery, serves it to the real handler **over a real HTTP connection**,
lets the real queue run the real pipeline against a fake GitHub and a fake
model, and asserts the comment that arrives — including that the committed key's
rule is named, that the model finding has a line number, that CRITICAL is
rendered above MAJOR, and that the credential's value never appears in the
comment.

## Repository layout

```
main.go                     serve, review and runs; the wiring
internal/webhook            the HMAC check, the event filter, the HTTP surface
internal/runner             the queue: one run per pull request, records, retries
internal/review             the pipeline, the merging, the comment policy
internal/lint               the linter runner, its parsers, the secret scanner
internal/diff               reading unified diffs and the lines they touch
internal/github             the REST client, pagination included
internal/model              the prompt, the reply parsing, the providers
internal/vcs                the shallow checkout of the head commit
internal/httpx              retries with backoff and Retry-After
internal/config             the configuration, its defaults and its validation
internal/notify             the failure notification
```

## Notes from building it

Six things were learned the hard way and are worth writing down.

**gofmt and `go vet` are not the same program in every Go release.** The first
run of the workflow failed the formatting check: gofmt 1.22 indents a multi-line
composite literal inside a `return` differently from gofmt 1.23+, so a file
formatted with the newer toolchain was rejected by the older one. The construct
was rewritten so both agree, and CI now runs the test suite on the newest Go as
well as on the declared minimum — because the same drift showed up in `go vet`,
which on Windows reports a diagnostic as `.\main.go:6:2` in Go 1.22 and
`main.go:6:14` in later releases. That difference was hiding a real bug: the
path normaliser trimmed the leading `./` before converting backslashes, so a
Windows tool's path became `./main.go`, matched nothing in the pull request, and
every finding from that linter was silently dropped.

**A tool that scans for credentials cannot contain credential-shaped strings.**
The first push of this repository was refused by GitHub's push protection,
because the scanner's own tests carried literals shaped like a Slack token and a
Stripe key. The samples are now assembled from pieces at run time — `"sk_" +
"live_" + …` — so no such literal exists in the tree, while the scanner still
sees exactly the string it is meant to catch. Push protection was right, and the
fix made the repository better rather than worse.

**The Go toolchain reports on stderr.** A lint runner that reads only stdout
reports `go vet` as clean, and a clean run looks exactly like a run that did not
happen. Every lint command therefore names the stream its output arrives on.

**The git transport does not accept a Bearer token.** `Authorization: Bearer`
works for the REST API and is answered with 401 by the smart HTTP endpoint,
after which git asks for a username — and a bot that set
`GIT_TERMINAL_PROMPT=0` fails with "could not read Username". The fix is Basic
authentication with the token as the password and `x-access-token` as the user.
This was found by running the thing against a real repository; no test double
would have shown it.

**A run must outlive the request that queued it.** A webhook handler's context
is cancelled the instant its response is written, so a review that inherited it
was cancelled by the very act of answering `202 Accepted`: every delivery
produced a "superseded" run that never read anything. The queue now detaches
from the caller's context, and the tests that cover this deliver over a real
HTTP connection, because serving a recorder in-process does not cancel anything
and hid the bug completely.

**A cancelled run must not be reviewed anyway.** When a new push supersedes a
queued run, the cancellation and the freeing of a work slot can arrive in the
same instant, and Go's `select` chooses between ready cases at random. The
cancellation is therefore checked again after the slot is acquired, so a
superseded revision never reaches the model. The test that covers it failed
about one run in five before the fix.

## License

MIT. See [LICENSE](LICENSE).
