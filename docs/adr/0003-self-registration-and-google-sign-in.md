# ADR 0003 — Self-registration and Google sign-in

**Status:** accepted · **Date:** 2026-08-14 · **Reverses:** [ADM-01](../03-frs.md) ("no self-service registration"), the built-in-only authentication decision in [docs/README.md](../README.md), and "No self-registration" in [docs/13-ui-design.md §13.2](../13-ui-design.md)

## Context

Three locked decisions said this would not happen:

- **ADM-01**: "User CRUD (create with temp password, edit role/groups, deactivate); **no self-service registration**."
- The stakeholder table records authentication as **built-in** (argon2id + JWT, optional TOTP).
- The UI design states "No self-registration, no public password reset (admin-driven)."

They were the right decisions for an internal tool with tens of users, where an administrator creating each account is a few minutes of work and a useful gate.

The stakeholder asked for both, and chose **open registration** with **automatic account creation** for Google identities when offered the alternatives (approval queue, domain allowlist, link-only). Decisions belong to whoever owns the product; this records the change rather than re-litigating it.

## Decision

Two new ways to get an account, both off unless switched on:

1. **Self-registration** — `POST /auth/register`, gated by the `auth.self_registration` setting, which ships **disabled**.
2. **Google sign-in** — OpenID Connect authorization code flow with PKCE, enabled by supplying client credentials in the environment.

Every self-provisioned account is created in a **`newuser` role with no group membership** — a role that reaches one page and nothing else.

## Rationale

### Why an empty account is the containment

Registration hands out an account, not access. Roles say what someone may do; grants say what they may do it to, and a new account has none — so its inventory is empty, its dashboard shows nothing, and every VM answers 404. An administrator granting a VM group is still the moment access begins, exactly as before.

That is what makes open registration tolerable: the gate moved from "can you have an account" to "can you see anything", and the second gate is the one that mattered.

**The role was originally read-only, and that was not empty enough.** Read-only can list the estate's hosts, storage and networks — none of which is scoped by grants, because they describe the infrastructure rather than any particular VM. A stranger who registered could therefore enumerate the nodes, pools and interfaces without ever being granted a thing. `newuser` closes that: it is declared on exactly two routes, `GET /auth/me` and `POST /auth/password`, so every other endpoint refuses it by the map's deny-by-default. The frontend matches — the router hands it a welcome page telling it to ask an administrator for access, rather than a navigation full of views it cannot load.

Granting access is now two steps for an administrator: change the role, then grant a VM group. That is a real cost, and it is the right one — the alternative gave away a survey of the estate to anyone who filled in a form.

### Where Google's credentials live

Configured in **Settings → Google sign-in**, with the environment as a fallback for deployments that would rather set it there (`PROXUI_GOOGLE_CLIENT_ID`, `PROXUI_GOOGLE_CLIENT_SECRET`, `PROXUI_GOOGLE_REDIRECT_URL`). A value in Settings wins.

This was initially environment-only, on the grounds that the settings table stores plain text and a client secret does not belong there. That reasoning was sound but the conclusion was wrong: the answer was to make the table able to hold a secret, not to keep the feature out of the UI. Settings gained a `secret` kind (migration 00012) using the same envelope encryption as platform credentials — a per-secret data key wrapped by the master key. A secret is write-only: never returned by any read, shown as "set" with a replace affordance, and audited as `{"secret_replaced": true}` rather than by value.

Configuration is re-read per request rather than captured at boot. The usual mistake here is a mismatched redirect URL, which is only discovered by trying it; correcting it should not need a restart.

### Why the flow is hand-written

It is one redirect and one token exchange. The parts worth getting right — state, PKCE, and verifying the identity token's **signature** against Google's published keys — are precisely the parts a framework hides, and the parts that decide whether anything that can reach the callback can claim to be anyone.

Specifically:

- **State** is held server-side in Redis and consumed with `GETDEL`, so a callback carrying a state nobody issued is refused, and a replayed callback finds nothing.
- **The PKCE verifier never leaves the server.** Only its SHA-256 goes to Google.
- **The identity token's signature is checked** against Google's JWKS, with issuer, audience and expiry enforced, and the key set refetched once on an unknown key id because Google rotates.
- **The nonce is checked**, tying the token to the redirect this portal started.
- **`email_verified` is required.** An unverified address could belong to someone else, and the address is what links a provider identity to a portal account.

### Lookup by subject, then email

Google's subject identifier is stable; an email address is not. Looking up by subject first means someone who changes their address keeps their account, and an address reassigned to a different person does not hand them the previous holder's access. Email is the fallback only for the first sign-in, where it links an account an administrator already created rather than creating a duplicate — so grants already attached to that account survive.

### Password accounts and provider accounts are disjoint

An account that signs in through a provider holds no password, and the password path refuses it on the provider rather than on the hash comparison. Failing on the comparison would report "wrong password" for a password that does not exist, which invites guessing.

## Consequences

- **Anyone who can reach the portal can create an account** while registration is open. They see nothing, but the account exists, appears in Users, and can sign in. Turning the setting off stops it immediately — the policy is read per request, not cached.
- The setting **fails closed**: if it cannot be read, registration is treated as disabled. A database hiccup opening the portal to new accounts is the wrong way round to be wrong.
- Registration is rate limited as strictly as sign-in, being the other endpoint reachable without an account.
- `users` gained `auth_provider` and `external_id`, unique together where the external id is present, so one Google account cannot become two portal accounts.
- `user_role` gained a `newuser` value (migration 00013). Postgres enum values cannot be removed, so this is one-way.
- **Read-only still sees hosts, storage and networks.** `newuser` means self-registration no longer hands that out, but an administrator who assigns read-only is still granting a survey of the estate. Whether those three views should be narrowed remains open, and is now a deliberate choice rather than a side effect of registering.

## Alternatives considered

- **Approval queue.** Offered and declined. It would have been the safer default, and the code is shaped so adding a mode is a setting value rather than a redesign.
- **Domain allowlist.** Offered and declined for the same reason.
- **A generic OIDC provider instead of Google specifically.** Rejected for now: Google's endpoints are hardcoded, which keeps startup free of a network call. Generalizing means moving three constants into configuration and a discovery fetch — worth doing when a second provider is actually wanted.
- **`golang.org/x/oauth2` plus an OIDC library.** Rejected: the dependency is larger than the code it replaces, and it would put the signature verification — the one part that must be right — behind an abstraction.

## Verification

Registration was exercised against the running portal: enabling the setting, registering, and confirming the new account signs in immediately and reaches nothing. Every route was then tried with its token — inventory, dashboard, hosts, storage, networks, platforms, users, groups, grants, audit logs, settings, alert rules, alerts, notification channels and rules, console sessions, system info, connectors, and a console on a VM that exists — and all twenty answered **403**. `GET /auth/me` answers 200 with `role: newuser`, and the password change endpoint is reachable; those two are the whole surface. Duplicate username and duplicate email return the same message, so the form cannot be used to ask which accounts exist.

**Google sign-in has been exercised against Google end to end**, once the credentials were entered in Settings: the authorize redirect is accepted, the callback exchanges and verifies an identity token, and the account is provisioned. What is unit-tested is what needs no credentials: the authorize URL carries state, nonce and a correct PKCE challenge while never carrying the verifier; an unconfigured client refuses; attempts are unique and long enough; malformed signing keys are rejected; and the return path cannot be used to redirect off the portal.
