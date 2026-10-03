# Security policy

devdash reads other processes' details (argv, working directory, open sockets), talks to the
Docker socket, and sends signals with `devdash kill` and the dashboard's kill action. A bug in
any of these can hurt the machine it runs on, so please report it privately.

## Supported versions

Only the latest release gets security fixes. Fixes land on `main` and ship in the next release.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting: open the repository's **Security** tab and choose
**Report a vulnerability**. Please do not open a public issue, pull request or discussion for a
security bug.

A useful report says which devdash version and OS you ran (`devdash version`), what you did,
and what happened. Examples of what counts:

- devdash signals a process it should refuse or did not plan to signal (another pid after pid
  reuse, pid 1, devdash itself, a container runtime), or signals without the confirmation the
  README describes.
- `--json`, the dashboard or an error message exposes data it should not, such as environment
  variables or another user's details beyond what the README says is shown.
- Input from the environment (`DOCKER_HOST`, a Docker context, `/proc` contents) makes devdash
  read or connect somewhere it should not.

You will get an answer in the advisory. Once a fix is released, the advisory is published with
credit to you unless you ask otherwise.
