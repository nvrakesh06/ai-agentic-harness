# GitHub Free

AIH supports ordinary public and private GitHub repositories with write access.
It needs issues, pull requests, and Git refs; it does not require Projects, native
sub-issues/dependency APIs, paid protections, hosted runners or native merge queues.

Parent/dependency relationships are recorded in issue bodies plus the durable
task DAG. Stable `<!-- aih:ID -->` markers reconcile creation after failures. PR
bodies carry acceptance/check/review/base/policy evidence. No high-frequency
issue-comment message bus is used.

The development machine or VM runs native verification, advisory workers and the serialized
merge train. `aih-state` stores logical orchestration; source branches store code.
Atomic Git updates protect current-base and controller comparisons. Remote policy
that disallows this integration strategy produces a blocker rather than being
silently weakened. This is not a server-enforced rule against other repository
writers pushing unsafe code directly.

Release assets can be built and published with local Go/Git/gh tooling; no Actions
workflow is installed. GitHub API quotas, network availability, provider usage fees
and local compute still apply. AIH itself does not create billing resources.
