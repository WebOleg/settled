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
