# Threads

> **Provided as is, without warranty.** If you deploy it, you are responsible
> for ensuring that your personal data and your users' data are sufficiently
> secured, including access controls, secrets, backups, and dependency updates.
> See the [MIT license](LICENSE).

Threads is a self-hosted chat app for humans and agents. One Cloudflare Worker
serves the Svelte client, HTTP API, WebSockets, and MCP endpoint. Each deployment
is one isolated instance with its own D1 database, R2 bucket, Durable Objects,
Workers AI binding, and users.

[![Deploy to Cloudflare](https://deploy.workers.cloudflare.com/button)](https://deploy.workers.cloudflare.com/?url=https://github.com/danielcorin/threads.space)

## Deploy

Click **Deploy to Cloudflare**, connect a Cloudflare account, and enter one
required secret:

- `INSTANCE_SECRET`: generate it with `openssl rand -base64 32` and keep it
  stable. It protects stored credentials and MFA secrets and is the temporary
  password used to claim a fresh instance.

Cloudflare creates the Worker, D1 database, R2 bucket, Durable Object classes,
Analytics Engine dataset, and AI binding from [`wrangler.jsonc`](wrangler.jsonc).
The deploy command builds and publishes the Worker (which lets Wrangler create
fresh resources), then applies the D1 baseline migration.

When the deployment finishes, open its URL and sign in with username `admin`
and password `INSTANCE_SECRET`. Threads atomically creates the first human admin
on that login. Change the password immediately in Settings. This bootstrap path
closes permanently as soon as a human user exists.

Email verification, feedback email, and Web Push are optional. Feedback email
also requires an instance-owned `FEEDBACK_EMAIL_TO`; Threads runs without these
integrations. See [the deployment runbook](docs/local-deploy.md).

## What is deployed

- One combined Cloudflare Worker and static asset bundle
- One D1 database with a clean initial schema
- One R2 upload bucket
- `ChatRoom`, `PresenceRoom`, and `UserEventsRoom` Durable Objects
- Optional Workers AI transcription and Analytics Engine message metrics

Each deployment is independent and contains only the standalone application
path. Users, admins, bots, channels, memberships, invitations, and MFA are all
scoped to that one Threads instance.

## Local development

Use Node.js 22 or 24 LTS and install dependencies from the repository root:

```bash
npm install
cp .dev.vars.example .dev.vars
# Fill INSTANCE_SECRET in .dev.vars
npm run db:migrate:local
npm run build
npx wrangler dev
```

The combined Worker defaults to Wrangler's local port. For separate API/client
development:

```bash
npm --prefix api run setup:local
npm --prefix api run dev       # http://localhost:8788
npm --prefix client run dev    # http://localhost:5173
```

`setup:local` seeds `dan` / `dan-password-123` plus the stable echo-bot fixture.
Customize it with `SEED_USERNAME`, `SEED_PASSWORD`, or `SEED_BOT=0`.

## Connect T3 Code

The agent bridge can route Threads bot invocations into native T3 conversations.
Each Threads root maps to one T3 thread; replies continue that conversation.
Tool activity and Stop actions flow through the bridge.

With T3 running and signed in, authorize the bridge from the repository root:

```bash
npm run bridge:t3:setup
```

This stores OAuth credentials in ignored `.local/t3-credentials.json` with owner-only permissions.
Copy `agent-tools/bridge/config.t3.example.json` to `agent-tools/bridge/config.local.json`.
Set the T3 project, provider, model, bot user, and channel IDs in that file.
Set `THREADS_T3_TOKEN` to a Threads bot token, then start the bridge:

```bash
npm run bridge -- -config config.local.json
```

The example uses T3's `approval-required` mode. Approve requests in T3.
See [the bridge setup guide](agent-tools/bridge/README.md#connect-t3-code) for modes, ports, and delivery limits.

## Validation

```bash
npm test
npm run check:threads
npm run build
```

The API and WebSocket source contracts live in `api/openapi/`. Generate and
check all derived contracts with:

```bash
npm --prefix api run openapi:bundle
npm --prefix api run openapi:clients
npm --prefix api run openapi:check
```

## CLI and MCP

The first-party `agent-tools/` package provides the CLI and the action catalog used
by the hosted MCP endpoint:

```bash
npm run threads -- --help
npm run threads -- --api http://localhost:8788 --token "$THREADS_TOKEN" whoami
```

Every combined instance exposes MCP at `https://<instance>/mcp` and the REST API
at `https://<instance>/api`. Both accept instance-issued API tokens. See
[the MCP contract](docs/mcp.md) and [agent integration guide](docs/agent-integration-guide.md).

## License

[MIT](LICENSE)
