# Automatr

Local-first email assistant for Microsoft Outlook that intelligently categorizes, archives, and summarizes your inbox using AI.

## Features

- 🤖 **Smart Classification** - Multi-layer classification using learned rules, embeddings, and LLM adjudication
- 📦 **Auto-Archive** - Automatically archive emails based on category rules (Marketing, Finance, Product Updates, etc.)
- 📝 **AI Summaries** - Action-focused email summaries that highlight questions, deadlines, and required actions
- 🔒 **Privacy-First** - Everything runs locally, no external storage (only API calls to Microsoft Graph and LLM providers)
- 💾 **SQLite Storage** - Local database with WAL mode for concurrent access
- 🔄 **Sync Command** - One command to fetch, classify, archive, summarize, and generate digest

## Quick Start

### 1. Set up Azure App Registration

- Go to [Azure Portal](https://portal.azure.com) → Azure Active Directory → App Registrations
- Create a new registration with redirect URI: `http://localhost:8383/callback`
- Required API permissions: `Mail.ReadWrite`, `MailboxSettings.ReadWrite`, `Mail.Send`, `offline_access`
- Note your **Client ID** and **Tenant ID**

### 2. Configure Automatr

Create `~/.automatr/config.yaml`:

```yaml
client_id: "your-azure-client-id"
tenant_id: "your-azure-tenant-id"
redirect_uri: "http://localhost:8383/callback"
scopes:
  - Mail.ReadWrite
  - MailboxSettings.ReadWrite
  - Mail.Send
  - offline_access
db_path: "~/.automatr/automatr.db"
```

Set your OpenAI API key:
```bash
export OPENAI_API_KEY="your-openai-api-key"
```

### 3. Build and Authenticate

```bash
go build -o automatr ./cmd/automatr
./automatr -auth
```

This will open your browser for Microsoft authentication and save the token locally.

### 4. Initialize Categories

Categories are created automatically on first run. Default categories:
- **Clients** (stay in inbox)
- **Ops** (stay in inbox)
- People, Finance, Auth, Marketing, Product Updates, Community, Projects, Later, Other (all auto-archived)

## Usage

### Sync Command (Recommended)

Fetch, classify, archive, summarize, and generate digest in one command:

```bash
./automatr -sync -sync-timespan 48h
```

This will:
1. Fetch emails from inbox for the last 48 hours
2. Classify any new messages
3. Auto-archive based on category rules
4. Generate AI summaries for all messages
5. Output a formatted digest

### Individual Commands

**Authenticate with Microsoft**
```bash
./automatr -auth
```

**List recent emails**
```bash
./automatr -list -list-timespan 7d
```

**Classify a single email (dry-run)**
```bash
./automatr -classify <message-id>
```

**Review and classify recent emails**
```bash
./automatr -review -review-days 2
```

**Generate digest**
```bash
./automatr -digest -digest-timespan 24h
```

**Run daemon (continuous monitoring)**
```bash
./automatr -daemon
```

### Timespan Format

All timespan parameters support Go duration format plus shortcuts:
- `24h`, `48h` - hours
- `7d`, `14d` - days (shortcut)
- `2w`, `4w` - weeks (shortcut)

## Architecture

```
cmd/automatr/          Main application entry point
internal/
  auth/                OAuth2 PKCE authentication
  graph/               Microsoft Graph API client
  classify/            Multi-layer classification engine
    - rules.go         Hardcoded rules (List-Unsubscribe, etc.)
    - embeddings.go    Semantic similarity matching
    - llm.go           GPT-4o-mini for final adjudication
    - learning.go      Learn from user corrections
  store/               SQLite database (WAL mode)
  summarize/           Email summarization with category-specific prompts
  daemon/              Background monitoring and review server
```

## Classification Layers

1. **Learned Rules** - User corrections stored and applied first
2. **Hardcoded Rules** - Pattern matching (e.g., `List-Unsubscribe` → Marketing)
3. **Embeddings** - Semantic similarity using OpenAI `text-embedding-3-small`
4. **LLM Adjudication** - GPT-4o-mini makes final category decision

## Auto-Archive Logic

Messages are automatically archived if:
- Category has `keep_in_inbox = false`
- Confidence > 0.6
- NOT from company domain (configurable)
- NOT flagged/high-importance
- NOT part of recently replied thread

## Summarization Prompts

Each category has customized prompts that emphasize:
- **ACTION NEEDED** prefix when questions or tasks exist
- Deadlines and time-sensitive information
- Questions that need responses
- Required actions and next steps

Example categories:
- **Clients**: Highlights client name, questions asked, deadlines
- **Ops**: Shows system/service, severity, and required action
- **Projects**: Notes task assignments and questions for you

## Development Status

✅ **Phase 1: Foundation** (Complete)
- OAuth2 PKCE authentication
- Microsoft Graph client
- SQLite storage with WAL mode
- Message fetching with delta sync

✅ **Phase 2: Classification** (Complete)
- Multi-layer classification engine
- Category management
- Learned rules from user feedback
- Auto-archiving

✅ **Phase 3: Production** (Complete)
- Email summarization with AI
- Digest generation
- Sync command for end-to-end workflow
- Daemon mode

## Database Fixes

This version includes critical SQLite concurrency fixes:
- WAL mode enabled for concurrent reads/writes
- Proper cursor management (no nested queries while holding cursors)
- Single connection pool to avoid deadlocks

## Documentation

- [CLAUDE.md](./CLAUDE.md) - Development context and architecture details
- [docs/prd.md](./docs/prd.md) - Full product requirements
- [docs/classification-strategy.md](./docs/classification-strategy.md) - Classification approach

## Tech Stack

- **Language**: Go 1.24.0
- **Database**: SQLite with WAL mode
- **APIs**: Microsoft Graph API, OpenAI API
- **Platform**: macOS (darwin/arm64)

## License

MIT
