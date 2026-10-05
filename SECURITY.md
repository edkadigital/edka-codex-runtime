# Security policy

Report a vulnerability in these images privately, through
[Report a vulnerability](https://github.com/edkadigital/edka-codex-runtime/security/advisories/new)
on this repository. Leave it out of public issues and pull requests.

Include the runtime version (the image tag), the authentication mode, and the steps that
show the problem. Leave out live credentials: GitHub tokens, OpenAI API keys, ChatGPT
`auth.json` files and connection tokens.

Fixes ship as a new `edka` revision of the runtime. New environments use it, and existing
environments adopt it with **Update Environment**.

[README.md](README.md#where-each-credential-lives) describes where each credential lives
and which container can read it.
