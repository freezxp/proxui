# 26. Setting up Google sign-in

How to create the OAuth client this portal needs, and what Google will not let
you do.

## 26.1 Read this before you start

**Google will not accept the address this portal is currently served at.**
Its rules for an authorized redirect URI are:

| Rule | Consequence here |
|---|---|
| Must use `https://`, except for localhost | `http://…:8080` is refused |
| No IP addresses, except `127.0.0.1` and `localhost` | `192.168.100.23` is refused |
| The domain must have a public suffix | `.vm`, `.local`, `.lan` and `.internal` are refused |
| For an *External* app, you must **own** the domain and verify it | an invented internal name cannot be used |

So `http://192.168.100.23:8080/api/v1/auth/google/callback` cannot be
registered, and neither can anything ending in `.vm`. This is Google's
restriction, not the portal's — there is no setting that works around it.

A name like `vm.intranet.my` clears the public-suffix rule, because `.my` is a
real top-level domain. It still has to be reached over `https://`, and for an
External app you have to prove you own `intranet.my` — see §26.2a.

Three ways forward:

### A. A real domain with HTTPS — the only option for a team

Point a public DNS name at the portal and terminate TLS in front of it. The
shipped compose file already includes Caddy for exactly this
([docs/14-deployment.md](14-deployment.md)); a `Caddyfile` of

```
vm.example.com {
    reverse_proxy proxui:8080
}
```

gets a Let's Encrypt certificate automatically. The name has to resolve
publicly for the certificate to be issued, but the portal itself can stay on
your own network — only the certificate check needs to reach it, and
DNS-01 validation avoids even that.

Redirect URI becomes `https://vm.example.com/api/v1/auth/google/callback`.

### A2. An internal-only name on a domain you own

The host does not have to be reachable from the internet. What has to be
public is enough of the DNS to get a certificate:

- Point `vm.example.com` at the portal's private address. A public `A` record
  holding `192.168.100.23` is perfectly legal and resolves to nothing useful
  from outside — or use split-horizon DNS and skip the public record
  entirely.
- Issue the certificate with **DNS-01** validation rather than HTTP-01. It
  proves ownership by writing a TXT record, so Let's Encrypt never needs to
  reach the host. Caddy supports this with a DNS provider module.

This is the usual shape for an internal portal that still wants real
certificates and Google sign-in.

### B. localhost, for trying it out

Google allows `http://localhost`. Forward the port from your own machine:

```bash
ssh -L 8080:127.0.0.1:8080 ubuntu@192.168.100.23
```

Then use `http://localhost:8080` in the browser, and register
`http://localhost:8080/api/v1/auth/google/callback` as the redirect URI.

This works for one person testing. It is not a deployment: everyone else
still reaches the portal by its real address, where Google sign-in will not
work because the redirect does not match.

### C. Don't use Google

Password accounts and self-registration work at any address. Google sign-in
is a convenience, not a requirement.

## 26.2a Behind a reverse proxy

Three things to get right when TLS terminates in front of the portal.

**Pass the scheme through.** The portal decides whether to send HSTS from
`X-Forwarded-Proto`, and builds the `https://` half of the Google redirect URI
from it. Caddy's `reverse_proxy` sets it; nginx needs
`proxy_set_header X-Forwarded-Proto $scheme;`. Without it the portal believes
it is serving plain HTTP, omits the header, and sends Google an `http://`
redirect URI that Google refuses.

**Pass the host through.** Caddy and cloudflared keep the original `Host`;
nginx needs `proxy_set_header Host $host;`. A proxy that rewrites it to an
internal address should send `X-Forwarded-Host` with the real one, which the
portal prefers when it is there (§26.3a).

**Secure cookies.** `PROXUI_SECURE_COOKIES` defaults to true and needs nothing
done to it behind TLS. It matters only for a portal reached **both** ways —
say `https://vm.example.com` through the proxy and `http://10.0.0.5:8080`
directly on the LAN, which is worth keeping as a way in when the proxy or its
DNS is the thing that broke.

| Setting | Over TLS | Over plain HTTP |
|---|---|---|
| `true` (default) | `Secure` | `Secure`, so the cookie is never sent back and sign-in fails |
| `false` | `Secure` | no flag, so LAN sign-in works |

The flag is decided per request from `X-Forwarded-Proto`, not once at boot, so
`false` does **not** mean "never Secure" — an HTTPS request gets the flag
either way. Set it false only if you actually sign in over the plain-HTTP
address; the setting cannot weaken the HTTPS path.

This also depends on the proxy passing the scheme through, above: without
`X-Forwarded-Proto` the portal cannot tell the two apart and treats everything
as plain HTTP.

**WebSockets must pass through** for consoles and live updates: Caddy handles
this automatically, nginx needs `proxy_set_header Upgrade $http_upgrade;` and
`proxy_set_header Connection "upgrade";`. A proxy that buffers or drops the
upgrade shows up as a console stuck on "Connecting…".

## 26.2 Creating the OAuth client

Once you have decided on the address from §26.1.

1. **Open the Google Cloud console** at <https://console.cloud.google.com> and
   create a project, or pick an existing one. The project is just a container;
   its name is not shown to people signing in.

2. **Configure the consent screen.** Under *APIs & Services → OAuth consent
   screen* (recent consoles show this as *Google Auth Platform → Branding*):

   - **User type**: *Internal* if you have Google Workspace and only want
     people in your organisation — this is the better choice when it is
     available, because nobody outside can sign in at all. Otherwise
     *External*.
   - **App name**: what people see on the Google consent screen. Use the
     portal's name.
   - **User support email** and **developer contact**: required by Google.
   - **Scopes**: add `openid`, `.../auth/userinfo.email` and
     `.../auth/userinfo.profile`. Nothing else. These three are
     non-sensitive, which means **no Google verification review is needed**.
     Asking for anything more starts a review process you do not want.

3. **If you chose External**, decide between:
   - **Testing**: only accounts you list as test users can sign in, up to 100.
     Fine for a small team, and requires nothing from Google.
   - **Production**: anyone with a Google account can sign in. With only the
     three scopes above, publishing needs no verification.

   Remember what open registration means here: with `auth.self_registration`
   set to *open*, anyone Google lets through gets a portal account. It will
   see nothing until you grant it VMs, but it exists.

4. **Create the client.** *APIs & Services → Credentials → Create credentials
   → OAuth client ID*:

   - **Application type**: *Web application*.
   - **Name**: anything; it is only shown in the console.
   - **Authorized redirect URIs**: add one for **every address the portal
     answers to** —

     ```
     https://your-portal.example.com/api/v1/auth/google/callback
     https://vm.intranet.my/api/v1/auth/google/callback
     ```

     Character for character. Google compares the whole string: a trailing
     slash, `http` instead of `https`, or a different port is a different URI
     and the sign-in fails with `redirect_uri_mismatch`.

     The portal sends whichever one matches the address the browser is on
     (§26.3a), so a name you do not register is a name Google will refuse to
     sign in at — while the others keep working.
   - **Authorized JavaScript origins**: not needed. This portal does the
     exchange server-side.

   For an *External* app, the domain must also appear under **Authorized
   domains** on the consent screen, and Google will ask you to verify it in
   Search Console (a DNS TXT record or an HTML file). *Internal* Workspace
   apps skip this.

5. **Copy the client ID and client secret.** The secret is shown once.

## 26.3 Entering them in the portal

**Settings → Google sign-in**, as an administrator:

| Field | Value |
|---|---|
| Client ID | ends in `.apps.googleusercontent.com` |
| Client secret | stored encrypted; shown afterwards only as "set" |
| Redirect URL | leave it empty — see §26.3a |

Nothing needs restarting. The sign-in page shows a **Sign in with Google**
button as soon as the client ID and secret are present.

## 26.3a One portal, several addresses

A portal is routinely reachable under more than one name: a LAN name, a public
one, a tunnel hostname. Google returns the browser to the redirect URI
verbatim, so a sign-in started at `vm.intranet.my` that comes back to
`vm.cyberjaya.pro` lands the session on a host the person is not looking at —
and their cookies are not there either.

So **leave the Redirect URL empty**. Each sign-in then returns to the address
that browser used, taken from the request (`X-Forwarded-Host` when a proxy
rewrote `Host`, and `X-Forwarded-Proto` for the scheme, which is what makes
this work behind Caddy or a Cloudflare Tunnel). The redirect field's
placeholder shows the value the address you are on right now resolves to.

The address is resolved once, when the sign-in starts, and recorded against the
attempt server-side. The token exchange sends Google that same string, which is
the half Google checks twice.

Registering each address with Google is still required, and is the whole of the
safety here: a forged `Host` header can only produce a redirect URI Google
already knows, and Google refuses anything else before the browser goes
anywhere.

Set the field only to pin one address — for a portal behind a proxy whose
public address it never sees. A pinned value wins over the request every time.

## 26.4 When it does not work

| What you see | Cause |
|---|---|
| No Google button | the client ID or secret is empty; `GET /api/v1/auth/methods` reports what the portal thinks |
| `redirect_uri_mismatch` on one address but not another | that address is not registered with Google, or the Redirect URL field pins a different one (§26.3a) |
| Google says "Access blocked: … invalid request" | the redirect URI does not match what you registered, exactly |
| Google says the app is not verified | External + Production with sensitive scopes. Use only the three scopes in §26.2 |
| Back at sign-in with "Google could not confirm that sign-in" | the token exchange failed; the portal log carries Google's own reason, usually `redirect_uri_mismatch` or a wrong client secret |
| "There is no account for that address…" | `auth.self_registration` is disabled and no account exists with that email. Either enable it, or create the account first — signing in with Google then links to it |
| It worked, and the portal is empty | expected. A new account has no grants. **Users & groups → grants** |
| Console stuck on "Connecting…" through a CDN | the proxy is not passing the WebSocket upgrade. Cloudflare does by default; verify with `curl -sI` that a `/ws/` request returns `101` rather than `200` |

## 26.5 What the portal does with the identity

For completeness, since "sign in with Google" hides a lot:

1. The browser is sent to Google with a random `state` and `nonce`, and a
   PKCE challenge. The verifier behind that challenge never leaves the server.
2. Google sends the browser back with a code. A `state` the portal did not
   issue, or one already used, is refused.
3. The portal exchanges the code for an identity token, then **verifies its
   signature** against Google's published keys, along with issuer, audience,
   expiry and the nonce.
4. `email_verified` must be true.
5. The account is found by Google's subject identifier, or by email on the
   first sign-in — which links an account you already created rather than
   duplicating it, so its grants survive.
6. A portal session is issued. Google is not consulted again until the next
   sign-in.

See [ADR 0003](adr/0003-self-registration-and-google-sign-in.md) for why each
of those steps is there.
