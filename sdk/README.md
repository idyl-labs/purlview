# Purlview Go SDK and account API

The packages under `sdk/` are Purlview's Go SDK: the models and wire contract
of version 1 of the account API, a client for it, and the server side of its
HTTP binding. The `purlview` CLI and daemon use them, and so can any other
program.

| Package | Contents |
| --- | --- |
| `resource` | Credential-free identities, public share state, canonical share references and their intrinsic validation. Imports no API, transport or service code. |
| `api` | Sign-in, credentials, share create/list/revoke and installation contracts: routes, version, JSON shapes, typed errors and the serialised examples in `api/testdata`. |
| `purlview` | `Client`: a context-aware client for every operation. |
| `apiserver` | The account API's HTTP binding: routing, authentication headers, request decoding and validation, response validation and the error envelope, in front of an injectable `Service`. |
| `internal/transport` | Bounded single-attempt HTTP exchanges, response limits, cancellation, safe errors and redirect refusal. |

Public resource state carries no credentials. A successful `VerifyLogin`
returns `api.InstallationCredential`; starting or resending a sign-in never
returns installation authority. `api.ShareAccess`, returned only to the
creator by create and list, pairs public state with the sensitive
distributable URL. Revocation returns public state only. A share's canonical
`origin` has no path, query or fragment; its `url` keeps the recipient token
of a secret link, for the creator to hand on.

The server validates authentication, ownership, permissions, expiry and
revocation on every operation. The structural validation in these packages
cannot prove any of them.

## Using the client

```go
client, err := purlview.New(purlview.Config{
	Endpoint:       "https://account.example.invalid",
	RequestTimeout: 10 * time.Second,
	Client:         "purlview/0.3.0",
	Credentials: func(ctx context.Context) (string, error) {
		return installationToken, nil // the application keeps the credential
	},
})
if err != nil {
	return err
}
defer client.CloseIdleConnections()
identity, err := client.Identity(ctx)
```

Every operation takes a context. `StartLogin`, `ResendLogin` and `VerifyLogin`
are anonymous and never read the credential source. No SDK operation reads the
environment, configuration files or a keychain, opens a browser, prints or
starts a process. `WithCredentials` returns a view with a separate credential
source that shares the transport.

```go
created, err := client.CreateShare(ctx, api.CreateShareRequest{
	Key:        attemptID, // keep it to reconcile this attempt, and only this one
	Target:     "http://localhost:3000/demo?version=2",
	TTL:        15 * time.Minute,
	Recipients: []string{"reviewer@example.invalid"}, // omit for a secret link
	PublicKey:  sharePublicKey,                        // the share's own Ed25519 key
})
// A created share is not ready yet: it is ready once it has docked.
listed, err := client.ListShares(ctx) // includes the creator's links
revoked, err := client.RevokeShare(ctx, api.RevokeShareRequest{
	Reference: resource.ShareRef{ID: shareID},
})
// A nil error alone does not confirm revocation: revoked.Outcome must be
// "confirmed".
```

HTTPS is verified as Go verifies it. A caller may supply its own transport,
for example to trust a test server's certificate. `AllowHTTP` permits
plaintext only to a loopback address or `localhost`, and the `Host` override is
likewise restricted to loopback endpoints; neither disables TLS verification.
The default transport does not read proxy environment variables. Endpoints are
origins, without credentials, paths, queries or fragments. There is no default
endpoint and no discovery.

## Serving the API

`apiserver.New(accountHost, service)` returns an `http.Handler` for the whole
API. A `Service` decides everything that needs state; a service that also
implements `apiserver.EmailService` or `apiserver.InstallationService` serves
those routes too, and embedding `apiserver.Unimplemented` answers every
operation it does not implement with `not_implemented`. A deployment wraps the
handler with its own rate limits and policy; `apiserver.WriteError` writes the
same error envelope.

The CLI's integration tests serve a synthetic platform through this handler,
so the client, the share engine and the commands are tested against the exact
binding.

## Version 1 over HTTP

The Go types and JSON tags in `api` and `resource`, with the examples in
`api/testdata`, define the wire format; there is no separate schema.

Every route is on the account host. Requests send
`Accept: application/json` and `Purlview-API-Version: 1`; POSTs send
`Content-Type: application/json`. Clients also send
`Purlview-Client: purlview/<version>` (`purlview.Config.Client`), so that the
service can refuse a version it no longer serves with `update_required` (426);
a request without it counts as older than any minimum. The versioned path is
enough for a caller that omits the version header; an unsupported header
version is refused. Results use HTTP 200, JSON, `Purlview-API-Version: 1`,
`Cache-Control: no-store` and `X-Content-Type-Options: nosniff`. The server
assigns every response an `X-Request-Id`, which the SDK keeps in its errors;
an identifier the client sends is ignored. No route sets a cookie.

| Method and path | Authority | Input | Result |
| --- | --- | --- | --- |
| POST `/api/v1/login/start` | Anonymous; refuses an Authorization header | `email`, `device_label` | `id`, `expires_at`, `resend_at` |
| POST `/api/v1/login/resend` | Anonymous | `id` | The same challenge, with a replacement code and the next resend time |
| POST `/api/v1/login/verify` | Anonymous | `id`, six-digit `code` | InstallationCredential |
| GET `/api/v1/identity` | `Authorization: Bearer <installation token>` | No body | Identity: account, account_id, device, device_label |
| POST `/api/v1/installation/revoke` | Installation bearer | `{}` | `outcome`, `already_ended` |
| GET `/api/v1/installations` | Installation bearer | No body | `installations`: id, label, created_at, last_used_at (optional), active_shares, current |
| POST `/api/v1/installations/revoke` | Installation bearer | `id` | `outcome`, `already_ended`, `id`, `shares_stopped` |
| POST `/api/v1/shares` | Installation bearer | `Idempotency-Key` header; target, targets (optional), ttl_nanoseconds, recipients (optional), rewrite_urls, public_key | `share` public state, sensitive distributable `url`, `invites` (optional), `tunnel` |
| GET `/api/v1/shares` | Installation bearer | No body | `shares`: an array of ShareAccess, `[]` when empty |
| POST `/api/v1/shares/revoke` | Installation bearer | `reference` with exactly one of `id` and `origin` | outcome, already_ended, public share, owner_notified |

Version 1 also defines a browser authorization flow,
`/api/v1/authorizations` and `/api/v1/authorizations/observe` (the client's
`BeginAuthorization`, `ObserveAuthorization` and `WaitAuthorization`). The
Purlview service does not offer it; sign in by email code.

Times are RFC 3339 timestamps. `ttl_nanoseconds` is an integer from
1,000,000,000 to 3,600,000,000,000 inclusive; a share's expiry never changes
and is at most one hour after creation. `recipients` lists up to ten email
addresses (`resource.MaxRecipients`); omitted or empty means a secret link.
Letter case and repeats are not significant: the share records the list in
lower case, without repeats, in the order given, and the SDK sends it that
way. The same key with an equivalent list is the same attempt; another order
or another set conflicts. `targets` names every target of a share with
several, the first included and equal to `target`'s origin, at most ten
(`resource.MaxTargets`).

A restricted link has no query; a secret link carries exactly one non-empty
`token` query value. The link opens the root of the share's entry host:
`https://<label>.<entry-domain>/?token=<secret>`, or the bare root for a
restricted share. `share.id` is the canonical `shr_<label>`; people read and
type the bare eight-character label, which is also the host label.
`share.entry_origin` identifies the entry host and `share.origin` the separate
content host. Revocation accepts either origin and never needs a recipient
token.

When the service emails recipients an invite with share creation, `invites`
reports one `{email, status}` entry per recipient, in `share.recipients`
order, with status `sent` or `not_sent`. The field is absent when no invites
were attempted. A replay of the same create returns the recorded results and
sends nothing again. Listed shares do not carry it.

`GET /api/v1/installations` returns the account's active installations,
oldest first; at most one is `current`, the installation that authenticated
the request. A caller that is not an installation sees none marked.
`last_used_at` is coarse, recorded about once an hour, and absent until first
recorded. `POST /api/v1/installations/revoke` revokes one installation of the
caller's account and ends its shares exactly as the installation's own
revocation does; `shares_stopped` counts the shares it ended, and
`already_ended` reports an installation revoked before. An id of another
account is `not_found`, like an unknown id.

`outcome: confirmed` on a revocation means authority has ended, the gateway
refuses access, and affected streams have closed. `unconfirmed` means that
guarantee is absent, even with HTTP 200. An HTTP 401 on installation revocation
is a refusal, not proof that the installation was already revoked.

### Shares and the edge

A create carries `public_key`: the share's own Ed25519 public key, 32 bytes,
base64 on the wire, generated by the daemon for that share; the service
refuses a create without it (`invalid_request`). The result carries `tunnel`:
the edge's `endpoint` (host:port, QUIC first with TCP fallback on the same
port), the exact SPIFFE IDs of the `edge` and the `gateway`, the share's `svid`
certificate chain (DER, leaf first) for that key, the admission `lease`
envelope, sent to the edge byte for byte, and the trust `bundle` (DER) that
authenticates the edge and the gateway. The SVID and the lease end with the
share. The daemon docks the share on the edge with this material, using
the Hyperplane Go client (`github.com/idyl-labs/hyperplane-go/dock`), and
serves the gateway's requests over end-to-end mutual TLS in which the share is
the server. The SDK refuses a reply whose tunnel does not match the request,
except that a replay which finds the share ended carries none. A listed share
never carries one.

### Errors

The error JSON has `error`, `message`, optional `request_id`, `outcome`,
`retryable`, optional `attempts_left` and optional `limit`. The outcome is
`not_applied` or `unknown`; neither means success. `attempts_left` comes only
with `denied` from login verify: how many more codes the challenge accepts, 4
down to 1. The fifth miss, and a lapsed or used challenge, are `expired`.
`limit` comes only with `denied` from share creation when an account limit
refused it: `running_shares` or `shares_per_hour`. It names the limit, never
its size, which differs by deployment and account; treat an unknown value as
an unnamed limit. The SDK ignores both fields on any other code.

| Status | Code |
| --- | --- |
| 400 | invalid_request, unsupported_version |
| 401 / 403 | unauthorised / denied |
| 404 / 405 | not_found / method_not_allowed (with Allow) |
| 409 / 410 | conflict / expired |
| 413 / 415 / 421 | request_too_large / unsupported_media_type / misdirected_request |
| 426 | update_required |
| 500 / 501 / 503 | internal_error / not_implemented / unavailable |

Unknown routes are 404; `/api/` versions other than v1, and incompatible
version headers, are 400. Unsupported methods are 405. A request for another
host is 421. Requests with unknown fields, trailing JSON, null or array bodies,
query strings, GET bodies or malformed structural input are refused before
any service call. Requests are bounded to 64 KiB and results to 1 MiB.
`apiserver` bounds a request to 30 seconds; SDK requests default to 10 seconds
and honour earlier deadlines.

Clients accept unknown response fields, keep unknown failure codes as errors,
and fail closed on unknown status values, missing required fields, malformed
or oversized JSON, a wrong HTTP status, media type or version, and trailing
data. Errors expose the API code, HTTP status, request ID, retry hint and
outcome, and support `errors.Is` for cancellation. Their text is the SDK's
own; it never includes server messages, transport errors or URLs. Redirects
are never followed, even to the same origin.

### Writes and retries

The SDK never retries a write, and net/http's body replay is disabled. A
failed dial is `not_applied`; once a write may have been sent, a lost or
invalid reply is `unknown`. `retryable` is a hint, not permission to repeat a
non-idempotent operation. Create keys are scoped to the authenticated
installation: the same key with the same inputs returns the same share, link
and original expiry, including for ended shares; different inputs conflict.
Two intentional invocations use different keys. A service must keep each key
and its result, or a tombstone, for as long as it offers reconciliation, and
must never silently recreate a forgotten attempt.

After an uncertain or cancelled create, the CLI's share engine makes one
bounded reconciliation with the same key and then revokes any share it finds.
If the reconciliation is refused or unreachable, the first write stays
unknown. Revocation is safe to repeat; it is complete only when confirmed.

### Sign-in

Codes are six random digits, leading zeros kept; `api.NormaliseLoginCode`
removes the spaces and dashes people type or paste (`482 913`, `482-913`)
before `LoginVerifyRequest` checks for exactly six digits. A code lasts five
minutes, and a challenge accepts five attempts and at most three sends, at
least a minute apart. A successful resend returns the unchanged `expires_at`
and the next `resend_at`; one that is too early or past the send limit is
`denied`, and a client tells the two apart with the `resend_at` it holds. A
lost verify reply needs a fresh sign-in. Installation tokens last 30 days;
signing in again with a still-valid credential keeps that installation, and
signing out locally forgets the token without revoking it.

## Tests

`go test ./sdk/...` runs the SDK's tests, including the round trip of every
example in `api/testdata` and the client against `apiserver` over real HTTP.
