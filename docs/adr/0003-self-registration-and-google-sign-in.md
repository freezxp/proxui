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

Every self-provisioned account is created **read-only with no group membership**.

## Rationale

### Why an empty account is the containment

Registration hands out an account, not access. Roles say what someone may do; grants say what they may do it to, and a new account has none — so its inventory is empty, its dashboard shows nothing, and every VM answers 404. An administrator granting a VM group is still the moment access begins, exactly as before.

That is what makes open registration tolerable: the gate moved from "can you have an account" to "can you see anything", and the second gate is the one that mattered.

### Why Google's credentials live in the environment

The settings table stores values in plain text. A client secret does not belong there, and adding an encrypted settings kind for one field would be a schema change carrying its own risk. Client credentials are deployment configuration in the same sense as the master key and the database URL: `PROXUI_GOOGLE_CLIENT_ID`, `PROXUI_GOOGLE_CLIENT_SECRET`, `PROXUI_GOOGLE_REDIRECT_URL`.

The cost is honest: an administrator cannot switch Google on from the UI, only from the deployment.

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
- **A self-registered account is read-only, and read-only currently sees hosts, storage and networks.** A stranger who registers can therefore enumerate the estate's nodes, pools and interfaces without ever being granted a VM. Restricting those three views to administrators and auditors would close it; that is a live decision, not something this ADR settles.

## Alternatives considered

- **Approval queue.** Offered and declined. It would have been the safer default, and the code is shaped so adding a mode is a setting value rather than a redesign.
- **Domain allowlist.** Offered and declined for the same reason.
- **A generic OIDC provider instead of Google specifically.** Rejected for now: Google's endpoints are hardcoded, which keeps startup free of a network call. Generalizing means moving three constants into configuration and a discovery fetch — worth doing when a second provider is actually wanted.
- **`golang.org/x/oauth2` plus an OIDC library.** Rejected: the dependency is larger than the code it replaces, and it would put the signature verification — the one part that must be right — behind an abstraction.

## Verification

Registration was exercised against the running portal: enabling the setting, registering, and confirming the new account is read-only, signed in immediately, sees **0 VMs**, and is refused the admin API. Duplicate username and duplicate email return the same message, so the form cannot be used to ask which accounts exist.

**Google sign-in has not been exercised against Google.** It needs a client ID, secret and a registered redirect URI, which are the stakeholder's to create. What is tested is everything that does not need them: the authorize URL carries state, nonce and a correct PKCE challenge while never carrying the verifier; an unconfigured client refuses; attempts are unique and long enough; malformed signing keys are rejected; and the return path cannot be used to redirect off the portal.
