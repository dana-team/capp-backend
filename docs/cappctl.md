# cappctl — CLI Reference Guide

`cappctl` is the command-line interface for the CAPP backend API. It manages Capp resources across multi-cluster Kubernetes/OpenShift environments.

---

## Building and Installing

```bash
# From the repo root
make build-cli          # produces bin/cappctl

# Or directly
go build -o bin/cappctl ./cmd/cappctl/
```

To embed a version string:

```bash
go build -ldflags "-X main.version=v1.2.3" -o bin/cappctl ./cmd/cappctl/
```

Add `bin/cappctl` to your `PATH`, or copy the binary to `/usr/local/bin/cappctl`.

---

## Configuration File

`cappctl` stores named contexts in:

```
~/.config/cappctl/config.yaml
```

The file is written atomically with permissions `0600`. You can override the path with `--config`.

### File Format

```yaml
current-context: my-cluster
contexts:
  - name: my-cluster
    server: https://capp.example.com
    auth-mode: jwt
    token: eyJ...
    refresh-token: eyJ...
    token-expires-at: "2025-06-01T12:00:00Z"
    cluster: production
    namespace: my-team
  - name: local
    server: http://localhost:8080
    auth-mode: passthrough
    token: dev-token
```

Fields per context:

| Field             | Description                                          |
|-------------------|------------------------------------------------------|
| `name`            | Unique context identifier                            |
| `server`          | Backend base URL                                     |
| `auth-mode`       | One of `passthrough`, `static`, `jwt`, `dex`, `openshift` |
| `token`           | Bearer access token                                  |
| `refresh-token`   | Refresh token (jwt/dex/openshift modes)              |
| `token-expires-at`| RFC3339 expiry time of the access token              |
| `cluster`         | Target cluster name                                  |
| `namespace`       | Default namespace                                    |

---

## Global Flags

These flags apply to every command.

| Flag            | Env var          | Default                              | Description                              |
|-----------------|------------------|--------------------------------------|------------------------------------------|
| `--config`      | —                | `~/.config/cappctl/config.yaml`      | Path to config file                      |
| `--context`     | —                | current context                      | Named context to use                     |
| `--server`      | `CAPP_SERVER`    | from context                         | Backend URL                              |
| `--token`       | `CAPP_TOKEN`     | from context                         | Bearer token                             |
| `--cluster`     | `CAPP_CLUSTER`   | from context                         | Target cluster name                      |
| `--namespace`   | `CAPP_NAMESPACE` | from context                         | Target namespace                         |
| `--output`, `-o`| —                | `table`                              | Output format: `table`, `wide`, `json`, `yaml` |
| `--insecure`    | —                | `false`                              | Skip TLS certificate verification        |

**Resolution order for server/token/cluster/namespace:** flag > environment variable > active context.

---

## Environment Variable Overrides

For CI/CD pipelines or scripting, you can bypass the config file entirely:

```bash
export CAPP_SERVER=https://capp.example.com
export CAPP_TOKEN=eyJ...
export CAPP_CLUSTER=production
export CAPP_NAMESPACE=my-team

cappctl get capps
```

---

## Authentication Modes

The server exposes its active auth mode at `GET /api/v1/auth/mode`. `cappctl login` auto-detects it if `--auth-mode` is omitted.

### passthrough

The token is used as-is as a bearer token on every API request. No server-side login call is made.

```bash
cappctl login --server https://capp.example.com --context dev --auth-mode passthrough --token <raw-token>
```

### static

Same as `passthrough` — the token is stored directly and sent as a bearer token. The server's static auth manager does not support a login endpoint, so no API call is made during login.

```bash
cappctl login --server https://capp.example.com --context staging --auth-mode static --token <static-token>
```

### jwt

Logs in via `POST /api/v1/auth/login` with a cluster-scoped token. Returns an access/refresh token pair. Refresh tokens are used transparently (see Token Refresh below).

```bash
cappctl login \
  --server https://capp.example.com \
  --context prod \
  --auth-mode jwt \
  --cluster production \
  --token <k8s-service-account-token>
```

### dex

Interactive username/password login via `POST /api/v1/auth/login`. The password is read securely (no echo). You can supply credentials via flags for scripted use (the `--password` flag is hidden from `--help` to reduce accidental exposure).

```bash
# Interactive (prompts for username and password)
cappctl login --server https://capp.example.com --context prod-dex --auth-mode dex

# Scripted
cappctl login --server https://capp.example.com --context prod-dex --auth-mode dex \
  --username alice --password hunter2
```

### openshift

OAuth2 device-like flow. `cappctl` fetches an authorization URL from the server, opens it in your browser (Linux: `xdg-open`, Windows: `rundll32`), then prompts you to paste the authorization code from the redirect URL.

```bash
cappctl login --server https://capp.example.com --context ocp --auth-mode openshift
# → Browser opens, paste the code when prompted
```

---

## Login

```
cappctl login [flags]
```

Authenticates and saves credentials to a named context. The context is set as current after a successful login.

**Required flags:**

| Flag          | Description                                 |
|---------------|---------------------------------------------|
| `--server`    | Backend base URL                            |
| `--context`   | Name to save this context under             |

**Optional flags:**

| Flag          | Description                                              |
|---------------|----------------------------------------------------------|
| `--auth-mode` | Authentication mode (auto-detected if omitted)           |
| `--token`     | Bearer / service-account token (passthrough/static/jwt)  |
| `--cluster`   | Target cluster (required for jwt mode)                   |
| `--namespace` | Default namespace to store in the context                |
| `--username`  | Username (dex mode)                                      |
| `--insecure`  | Skip TLS certificate verification                        |

**Examples:**

```bash
# Auto-detect auth mode
cappctl login --server https://capp.example.com --context prod

# JWT with explicit cluster
cappctl login --server https://capp.example.com --context prod \
  --auth-mode jwt --cluster mycluster --token $SA_TOKEN

# Store a default namespace
cappctl login --server https://capp.example.com --context prod \
  --auth-mode passthrough --token $TOKEN --namespace my-team
```

---

## Logout

```
cappctl logout [--context <name>]
```

Clears stored credentials (token, refresh token, expiry) from a context without deleting the context entry. Useful for invalidating a session without losing server/cluster settings.

```bash
cappctl logout                     # clears current context
cappctl logout --context staging   # clears a specific context
```

---

## Context Management

### List contexts

```
cappctl context list
```

Prints all configured contexts. The active context is marked with `*`.

```
* prod    https://capp.example.com   jwt
  staging https://staging.example.com dex
  local   http://localhost:8080       passthrough
```

### Switch context

```
cappctl context use <name>
```

Sets the named context as current and saves the config.

```bash
cappctl context use staging
```

### Print current context

```
cappctl context current
```

Prints the name of the active context, or `(none)` if unset.

---

## Token Refresh

For auth modes that return refresh tokens (`jwt`, `dex`, `openshift`), `cappctl` automatically refreshes the access token before every request when the token expires within 30 seconds.

Refresh flow:
1. `PersistentPreRunE` reads the active context's `token-expires-at`.
2. If `time.Until(expiry) <= 30s`, it calls `POST /api/v1/auth/refresh` with the refresh token.
3. The new token pair is saved back to the config file.
4. The refreshed token is used for the current request.

If the refresh token is set but `token-expires-at` is zero (unknown expiry), a warning is printed to stderr and the existing token is used without refreshing:

```
warning: refresh token set but token expiry unknown; skipping refresh
```

---

## Output Formats

Controlled by `--output` / `-o`. Valid values:

| Format  | Description                                           |
|---------|-------------------------------------------------------|
| `table` | Human-readable aligned table (default)                |
| `wide`  | Table with additional columns (e.g. UID for capps)    |
| `json`  | Indented JSON                                         |
| `yaml`  | YAML                                                  |

```bash
cappctl get capps --output json
cappctl get capps -o yaml
cappctl get capps -o wide
```

An invalid format value is rejected immediately with an error message before any API call is made.

---

## Capps

Capps are the primary resource managed by cappctl. All capp commands require `--cluster` (from flag, env `CAPP_CLUSTER`, or context).

### Get / List

```
cappctl get capps [name] [flags]
```

Without a name: lists all Capps. With a name: fetches a single Capp (requires `--namespace`).

**Flags:**

| Flag               | Description                          |
|--------------------|--------------------------------------|
| `-A`, `--all-namespaces` | List across all namespaces (no --namespace needed) |

**Examples:**

```bash
# List capps in current namespace
cappctl get capps --cluster prod --namespace my-team

# List all capps across all namespaces
cappctl get capps --cluster prod -A

# Get a single capp by name
cappctl get capps my-app --cluster prod --namespace my-team

# JSON output
cappctl get capps --cluster prod -A -o json
```

Table columns: `NAME`, `NAMESPACE`, `IMAGE`, `STATE`, `SCALE-METRIC`, `AGE`
Wide adds: `UID`

### Create

```
cappctl create capps [flags]
```

**Required flags:** `--name`, `--image`, `--cluster`, `--namespace`

**Optional flags:**

| Flag              | Description                                             |
|-------------------|---------------------------------------------------------|
| `--scale-metric`  | Scale metric: `concurrency`, `cpu`, `memory`, `rps`, `external` |
| `--state`         | Initial state: `enabled` or `disabled`                  |
| `--min-replicas`  | Minimum replica count                                   |
| `--container-name`| Container name                                          |
| `--env`           | Environment variable `KEY=VALUE` (repeatable)           |

**Example:**

```bash
cappctl create capps \
  --cluster prod \
  --namespace my-team \
  --name my-app \
  --image ghcr.io/myorg/my-app:v1.2.3 \
  --scale-metric cpu \
  --state enabled \
  --min-replicas 1 \
  --env DB_HOST=postgres \
  --env DB_PORT=5432
```

### Update

```
cappctl update capps <name> [flags]
```

Fetches the current Capp state, applies only the changed flags, and sends a PUT. Fields not specified on the command line are preserved.

**Optional flags:** same as create (except `--name`, which is the positional argument).

**Example:**

```bash
# Update image only
cappctl update capps my-app --cluster prod --namespace my-team --image ghcr.io/myorg/my-app:v1.3.0

# Scale down and disable
cappctl update capps my-app --cluster prod --namespace my-team --state disabled --min-replicas 0

# Replace all env vars
cappctl update capps my-app --cluster prod --namespace my-team --env DB_HOST=newhost --env DB_PORT=5433
```

### Delete

```
cappctl delete capps <name> [flags]
```

Prompts for confirmation before deleting. Use `--yes` / `-y` to skip.

**Flags:**

| Flag       | Short | Description                  |
|------------|-------|------------------------------|
| `--yes`    | `-y`  | Skip confirmation prompt     |

**Example:**

```bash
cappctl delete capps my-app --cluster prod --namespace my-team
# → Delete Capp "my-app" in namespace "my-team" on cluster "prod"? [y/N]

cappctl delete capps my-app --cluster prod --namespace my-team -y
```

---

## Insecure TLS

To connect to a server with a self-signed or untrusted certificate, pass `--insecure`:

```bash
cappctl login --server https://local.dev:8443 --context dev --insecure --auth-mode passthrough --token test
cappctl get capps --cluster local --insecure
```

The flag must be passed to every command (it is not saved to the context). For persistent insecure access, set it in a shell alias or wrapper script.

---

## Quick Start

```bash
# 1. Log in
cappctl login --server https://capp.example.com --context prod \
  --auth-mode jwt --cluster mycluster --token $SA_TOKEN --namespace my-team

# 2. Check current context
cappctl context current

# 3. List all Capps
cappctl get capps -A

# 4. Create a Capp
cappctl create capps --name hello --image nginx:latest

# 5. Update the image
cappctl update capps hello --image nginx:1.27

# 6. Delete the Capp
cappctl delete capps hello -y

# 7. Log out
cappctl logout
```
