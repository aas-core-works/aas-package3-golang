# Introduction

This series of articles helps you set up the development environment, explains how to test and check your code contribution, and how to properly submit it.

If you don't like reading the documentation and just want to take a deep dive and start contributing, the following section "Quick Start" gives you a brief overview of how you can get your code in.

## Quick Start

This is a brief list of steps explaining how to submit your code contribution.

### Development Tools

* Install Go 1.18 or later from [golang.org](https://golang.org/dl/)
* Install the IDE of your choice, *e.g.*, [Visual Studio Code](https://code.visualstudio.com/) with the [Go extension](https://marketplace.visualstudio.com/items?itemName=golang.Go)

### Create a feature branch

**If you are a member of [aas-core-works GitHub organization]**:

[aas-core-works GitHub organization]: https://github.com/aas-core-works

* Clone the Git repository:
  ```bash
  git clone https://github.com/aas-core-works/aas-package3-golang
  ```
  
* Create your feature branch:
  ```bash
  git checkout -b yourUsername/Add-some-new-feature
  ``` 
  
  Please observe our guideline to naming the branches: `{your-username}/{Describe-subject-of-the-commit}`.

**Otherwise**:

* Make a fork of the repository (see [this GitHub guide about forking])

[this GitHub guide about forking]: https://help.github.com/en/github/getting-started-with-github/fork-a-repo

* Clone your forked repository:
  ```bash
  git clone https://github.com/yourUsername/aas-package3-golang
  ```

* Create your feature branch:
  ```bash
  git checkout -b yourUsername/Add-some-new-feature
  ``` 

### Write Your Code

* Make your code changes
* Do not forget to implement unit tests
* Run the tests to make sure everything passes:
  ```bash
  go test ./...
  ```

### Commit & Push

* Format your code:
  ```bash
  go fmt ./...
  ```

* Run linting (if you have golangci-lint installed):
  ```bash
  golangci-lint run
  ```

* Add files that you would like in your pull request:
  ```bash
  git add package.go
  ```

* Commit locally:
  ```bash
  git commit
  ```

  Please observe our guideline related to commit messages:
  1) First line is a subject, max. 50 characters, starts with a verb in imperative mood
  2) Empty line
  3) Body, max. line width 72 characters, must not start with the first word of the subject

* Run the tests one more time to make sure everything passes:
  ```bash
  go test ./...
  ```

* If needed, change your commit message:
  ```bash
  git commit --amend
  ```

* Push to remote:
  ```bash
  git push origin yourUsername/Add-some-new-feature
  ```

* Create a pull request on GitHub
