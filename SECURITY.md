# Security

## Gondola's posture

Gondola is designed for a **single operator on a trusted LAN** — see
the threat model in [DESIGN.md](DESIGN.md). It is not hardened for
direct internet exposure, and it should not be published to the
internet without a reverse proxy, authentication, and TLS in front
of it.

Two settings matter:

- **Admin token.** If `server.admin_token` is empty, the `/admin/*`
  endpoints and mode changes are open to anyone who can reach the
  port. Set a token if anything you don't control shares the network.
- **Header forwarding.** Gondola forwards client headers to the
  serving backend by design; backends should treat gondola as a
  trusted caller.

## Reporting a vulnerability

Open an issue titled `security: <short description>` **without**
exploit details, saying you have a report to share privately, and the
maintainer will follow up with a private channel. Please don't post
working exploit details in a public issue.
