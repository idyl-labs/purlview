# Security policy

Please report security vulnerabilities privately, never in a public issue,
pull request or discussion.

## Reporting

Use GitHub's private vulnerability reporting: on this repository's
**Security** tab, choose **Report a vulnerability**. The report is visible
only to you and the maintainers.

Include what you can of:

- the affected component (the CLI, the daemon, the SDK or an installer) and
  version (`purlview --version`);
- the operating system and how Purlview was installed;
- steps to reproduce, and what an attacker gains;
- any proof of concept.

We acknowledge every report, keep you informed while we work on it, and
credit you in the advisory unless you prefer otherwise. Please give us a
reasonable time to release a fix before you disclose the issue.

## Scope

This repository's code: the `purlview` executable and its daemon, the Go SDK
under `sdk/`, and `install/install.sh` and `install/install.ps1`. Issues in
the Purlview service itself (purlview.com, the account API and the edge) are
welcome through the same channel.

Security fixes ship in a new release, and the update notice tells installed
copies about it.
