# Development Workflow

We develop with GitHub using pull requests (see [GitHub's guide on pull requests] for a short introduction).

[GitHub's guide on pull requests]: https://guides.github.com/introduction/flow/

**Development branch**. The development branch is always `main`.

**Releases**. The releases mark the development milestones on the `main` branch with a certain feature completeness.

## Pull Requests

**Feature branches**. We develop using feature branches, see [this section of the Git book].

[this section of the Git book]: https://git-scm.com/book/en/v2/Git-Branching-Branching-Workflows

If you are a member of the development team, create a feature branch directly within the repository.

Otherwise, if you are a non-member contributor, fork the repository and create the feature branch in your forked repository.
See [this GitHub tutorial] for more guidance.

[this GitHub tutorial]: https://help.github.com/en/github/collaborating-with-issues-and-pull-requests/creating-a-pull-request-from-a-fork

**Branch Prefix**. Please prefix the branch with your GitHub user name (*e.g.,* `mristin/Add-some-feature`).

**Continuous Integration**. GitHub will run the continuous integration (CI) automatically through GitHub Actions.
The CI includes building the library, running the tests, and checking code formatting.

## Commit Messages

The commit messages follow the guidelines from https://chris.beams.io/posts/git-commit:

* Separate subject from body with a blank line
* Limit the subject line to 50 characters
* Capitalize the subject line
* Do not end the subject line with a period
* Use the imperative mood in the subject line
* Wrap the body at 72 characters
* Use the body to explain *what* and *why* (instead of *how*)

Here is an example commit message:

```
Add stream-based package creation

Adding CreateInStream allows users to create packages directly into
memory buffers or network streams without writing to disk first.

This is useful for web services that need to generate AASX packages
on the fly.
```
