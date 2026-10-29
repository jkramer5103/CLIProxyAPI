# Optional banked reset expiry worker

`upstream.claude.auto-apply-banked-resets` is false by default. The server
advertises false in full v8 JSON even if YAML omits the field. The ordinary v8
scalar config route handles validation, persistence, ETags, and reload.

When enabled, one server worker checks all enabled Claude OAuth credentials once
per minute. It makes no provider calls when disabled. It only selects explicit
usable, eligible, non-paused grants with remaining resets and a known expiry in
`(now, now + 5 minutes]`, after their start and cooldown. Missing usability refuses
spending; missing `use_requires_limit` requires a current limit. Each attempt
fetches status, a fresh profile organization UUID, and status again. Successful
claims wait for the next poll and fresh limits before another reset is considered.

The worker reuses management quota token resolution/proxy selection and the
manager's existing refresh path after a read-only 401. It never retries a POST,
follows a redirect, or logs request/response bodies. A config change cancels
preflight. Credential session changes, disabling, and removal invalidate dispatch;
dispatch shares the manager's credential mutation lock. Manual v8 reset claims
share the journal/serialization with automation. Legacy v0 API calls are unchanged.

`banked-reset-journal/`, beside the config file, is a local 0700 directory with
0600 atomic, fsynced account records. File locking coordinates processes sharing
this directory; separate installations must not independently automate the same
account. A pending intent is durable **before** dispatch. Transport/read failures,
unknown responses, process crashes, and failed settlement writes leave the account
paused across restarts. No fresh claim ID or automatic spend retry follows an
uncertain claim. Known terminal answers/HTTP refusals permit another attempt no
sooner than one minute later. A corrupt/unwritable journal fails closed.

To reconcile a paused account, keep automation off, inspect the saved request ID
and upstream account state through an authorized manual process, and retain the
journal as evidence. There is deliberately no automatic reconciliation or
unpause endpoint. Never delete pending records merely to retry. The UI may close;
the worker belongs to the server lifecycle. No live redemption is required for
the deterministic tests.
