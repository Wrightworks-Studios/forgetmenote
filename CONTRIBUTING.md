# Contributing

Read [the architecture](docs/architecture.md), [engineering rules](AGENTS.md) and
[development skill](SKILL.md) before changing the application. Notebook content
and permissions share one boundary; credentials and execution state stay private.

For the application, install Go 1.27.4, Git and Node 24, then run:

```sh
npm ci
npm run build
go run ./cmd/forgetmenote -data ./data -url http://localhost:8080
```

An unbuilt frontend reports the required build step instead of a missing embed.
Python 3.12 and a virtual environment are needed for operator tests; the full release
gate also needs Docker and Playwright's Chromium and WebKit (`make setup-browser`). See
[verification](docs/verification.md) for setup, platform requirements and retained
failure evidence.

Write a small behavior test before implementing a behavior change. Use real
temporary Git repositories and synthetic HTTP/providers; never use personal
notebooks, production credentials or paid AI calls in the test suite. Run focused
checks during development, `make check-fast` for PR feedback and the canonical
full gate for shared, security, storage, dependency or multi-feature releases.

Describe the problem, resulting behavior and actual verification in a pull request.
Preserve unrelated work. Report security issues through the private channel in
[SECURITY.md](SECURITY.md). Publication and deployment require authorization.
ForgetMeNote is licensed under the [Functional Source License, Version 1.1, ALv2 Future
License](LICENSE.md): any use except a competing commercial product or service, and
Apache 2.0 two years after each release.
