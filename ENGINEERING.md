# Engineering rules

These rules apply to everything in this repository. A section is added in the same
commit as the first code it governs, so this file never describes code that does not
exist.

## Principles

1. Facts first. A version, a number, a command or a claim about how something works is
   checked in the code, the upstream docs or a real run before it is written down.
   Anything not checked is marked `not verified`.
2. Built as for production. A shortcut is either fixed before the commit or written
   down as a known limit with its reason.
3. Local first. Everything runs on one machine and needs no cloud account.
4. Proof over promise. Every claim in the README links to the test, output, dashboard
   or post-mortem that shows it.
5. One standard way per kind of thing. When a way to do something exists, it is used.
   A second way is not added next to it.
6. A change leaves the next change easier. A fix that only works for the case in
   front of it is not finished.
7. Scope is explicit. What the project deliberately does not do is listed in the
   README, so a missing feature reads as a decision.

## Documentation

- A document describes only what exists in the repository now. Plans live only in
  [ROADMAP.md](ROADMAP.md).
- Plain English: short sentences, concrete words, no marketing.
- A document is added in the same commit as the thing it describes, and changed in
  the same commit as the behaviour it describes.
- One fact lives in one place. Other documents link to it instead of copying it.
- Every number, command or behaviour can be checked against code, a test or output.
- No secrets, no addresses that hold real funds, no paths from a personal machine.

## Commits and history

- One commit is one change. The history of `main` reads as a log of the project.
- The subject line is `type: summary`, at most 72 characters. Types: `feat`, `fix`,
  `docs`, `test`, `refactor`, `chore`.
- The body has three parts: why the change was made, what changed by behaviour, and
  how it was tested.
- Pushed history on `main` is never rewritten.
- Commits are authored with the GitHub noreply address.

## Security baseline

- Secrets never enter git, logs, command output or documents. They are read from the
  OS keychain at run time.
- `.gitignore` keeps env files, key files and local runtime data out of git.
- A secret that reaches a commit is treated as leaked: it is rotated, not only removed
  from history.
- Every container image is pinned by digest; the tag is kept for the reader. A tag
  can be moved to other content, a digest cannot. A digest is added only after it was
  compared with the one the registry serves for the tag.
- Key files are created readable by their owner only (mode 600, in a directory with
  mode 700) and are never overwritten. A key file that others can read is refused, not
  fixed silently.

## Local checks

`make check` runs every check. A commit is pushed only after it passes, and CI runs the
same `make check` again on every push to `main` and every pull request
([.github/workflows/check.yml](.github/workflows/check.yml)). `make help` lists the
targets.

| Target | Tool | Fails on |
|---|---|---|
| `check-docs` | [scripts/check-docs.sh](scripts/check-docs.sh) | CRLF line endings, a missing final newline, trailing spaces outside Markdown, an em dash in Markdown, a relative link to a file that does not exist |
| `check-images` | [scripts/check-images.sh](scripts/check-images.sh) | a container image in a compose file, a script or the Makefile that is not pinned by digest |
| `check-shell` | shellcheck | any finding in a tracked shell script |
| `check-secrets` | gitleaks | a secret anywhere in the git history; findings are printed redacted |
| `check-go` | gofmt, go vet, go test | an unformatted file, a vet finding or a failing test |

The checks run with gitleaks 8.30.1, shellcheck 0.11.0 and Go 1.26.2, locally and in
CI. CI installs shellcheck and gitleaks with
[scripts/ci/install-tools.sh](scripts/ci/install-tools.sh), which checks each release
archive against its sha256 first, and pins every GitHub action by commit SHA. A tool is
upgraded in both places in one commit.

## Go

- One module at the repository root, `github.com/WebOleg/settled`. Commands live in
  `cmd/<name>`, packages in `internal/<name>`, one job per package.
- Dependencies are pinned in `go.mod` and `go.sum`. go-ethereum is pinned to the same
  release as the devnet's execution client.
- Errors are wrapped with what was being done:
  `fmt.Errorf("blobtx: compute cell proofs: %w", err)`. Callers branch on sentinel
  errors with `errors.Is`.
- Comments say why, not what.
- Every behaviour has a test, including the bad case. Tests that need a running network
  are commands run by `make`, not `go test`.
- Amounts of ETH are `*big.Int` or `uint256.Int` in wei. Never floats.
- Secrets reach a program through the environment or a file at run time and are never
  printed. The devnet mnemonic is public, and is still handled this way so the code
  path is the same one real keys will take.
