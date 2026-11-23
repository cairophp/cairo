# Security policy

## Supported versions

Cairo is before 1.0, and security fixes go into the newest release only.
Upgrade to it to get them; [CHANGELOG.md](CHANGELOG.md) says what each
release changes, and
[Stability](https://github.com/cairophp/docs/blob/main/stability.md) says
what a release may change.

## Reporting a vulnerability

Report it privately, not in a public issue: open the repository's
**Security** tab and choose **Report a vulnerability**
([direct link](https://github.com/cairophp/cairo/security/advisories/new)).
Say which version or commit you tested, what an attacker can do, and how to
reproduce it; a small `server.php` helps most.

You should have an answer within seven days. If the report is confirmed, the
fix is prepared in private, released as a new version, and published with a
security advisory that credits you unless you prefer otherwise. Please give
us until that release before you publish details.

## What counts

Anything in this repository and in the release binary and the official image
as we build them: the Go servers, the C bridge, the PHP runtime, the PDO
pgsql driver and `cairo gen`. Examples: a request that crashes the process,
reads memory or files it should not, crosses from one request's state into
another's, or gets past a limit such as `maxBody`.

A bug in PHP itself or in one of its extensions belongs with
[PHP's security team](https://www.php.net/security/), and a bug in an
application that runs on Cairo with that application. If you are not sure
where a problem lies, report it here and we will sort it out.
