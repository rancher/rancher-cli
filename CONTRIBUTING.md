# Contributing

We are not accepting code contributions at the moment. A clear issue is the
most useful thing you can send.

## Report a bug

Use the [bug report form](https://github.com/rancher/rancher-cli/issues/new?template=bug.yml)
and include:

- the rancher-cli version (`rancher --version`)
- the Rancher version, and whether it is community or Prime
- your operating system, architecture and shell
- the exact command you ran
- the full output, if the command printed an error: run the command again
  with `-o json` and paste the whole error document (code, message and
  hint) and the exit code
- what you expected to happen
- steps that reproduce it from a clean config directory:
  `RANCHER_CLI_CONFIG_DIR=$(mktemp -d) RANCHER_CLI_CREDENTIAL_STORE=file rancher auth login ...`
  (on Windows, set `RANCHER_CLI_CONFIG_DIR` to a new empty directory and
  `RANCHER_CLI_CREDENTIAL_STORE` to `file`)

For a problem with the install script or the documentation, write n/a in a
field of the form that does not apply.

Remove tokens, passwords and server URLs that must stay private from
everything you paste.

## Request a feature

Use the [feature request form](https://github.com/rancher/rancher-cli/issues/new?template=feature.yml).
Describe the workflow you are trying to do and how you do it today, not
only the flag or command you want.

## Report a security issue

Never in a public issue: see [SECURITY.md](SECURITY.md).

## Ask a question

Use the [Rancher forums](https://forums.rancher.com/) or
[Rancher Slack](https://slack.rancher.io/).
