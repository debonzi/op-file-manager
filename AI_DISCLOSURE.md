# AI-Assisted Development Disclosure

`opfm` is developed with assistance from AI agents. They may be used for
architecture discussions, implementation, tests, documentation, and routine
maintenance.

## Human accountability

AI agents are development tools, not maintainers. A human maintainer directs
their work, reviews changes before accepting them, runs appropriate validation,
and remains responsible for every merged change and published release.

## Sensitive information

This project manages sensitive files, so agents must never receive or expose
1Password session tokens, credentials, API keys, Kubernetes configurations,
Document contents, or other secret material. Those values must not appear in
prompts, source code, tests, issues, pull requests, screenshots, logs, or
commit messages.

## Contributions

Contributions are assessed using the same quality, security, licensing, and
maintainability standards regardless of the tools used to prepare them. This
disclosure does not require contributors to reveal AI assistance; it simply
describes how the project itself is developed.
