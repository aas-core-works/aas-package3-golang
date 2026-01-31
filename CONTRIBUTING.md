# Contributing

We welcome contributions to aas-package3-golang!

Please see the [Contributing Guide](doc/contributing/intro.md) for detailed information on:

- [Quick Start](doc/contributing/intro.md#quick-start) - How to get your code in
- [Development Workflow](doc/contributing/development-workflow.md) - Git workflow and pull requests
- [Style Guide](doc/contributing/style-guide.md) - Coding conventions
- [Testing](doc/contributing/testing.md) - How to write and run tests
- [Continuous Integration](doc/contributing/continuous-integration.md) - CI setup and checks
- [Releasing](doc/contributing/releasing.md) - Release process

## Quick Summary

1. Fork the repository (or create a feature branch if you're a member)
2. Make your changes
3. Run checks locally:
   ```bash
   go fmt ./...
   go vet ./...
   go test -race ./...
   ```
4. Commit with a [proper commit message](doc/contributing/development-workflow.md#commit-messages)
5. Open a pull request

## Code of Conduct

Please be respectful and constructive in all interactions.

## Questions?

If you have questions, feel free to open an issue for discussion.
