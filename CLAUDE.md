# Automatr - Development Context

## Overview
Local-first email assistant for Outlook (Microsoft 365) that categorizes, labels, and archives emails, then delivers twice-daily digests. Built in Go 1.24.0 for macOS (darwin/arm64).

## Key Principles
- **Local-first**: No external backend, all data stored locally
- **Privacy**: No third-party storage, tokens only
- **Non-destructive initially**: Read-only until explicit permission

## Architecture Components

```
internal/auth        - PKCE OAuth2 flow + token persistence
internal/graph       - Microsoft Graph API wrapper
internal/classify    - Rules + embeddings + LLM adjudicator
internal/store       - SQLite access + migrations
internal/summary     - Digest generation (HTML + plaintext)
internal/scheduler   - Cron-based job runner
cmd/automatr/main.go - Main entry point
```

## Authentication (OAuth2 PKCE)
- Redirect URI: `http://localhost:8383/callback`
- Scopes: `Mail.ReadWrite`, `MailboxSettings.ReadWrite`, `Mail.Send`, `offline_access`
- Token storage: `~/.automatr/token.json`

## Database Schema (SQLite)

```sql
-- State tracking (delta tokens, etc)
CREATE TABLE state (
  key TEXT PRIMARY KEY,
  value TEXT
);

-- Message metadata
CREATE TABLE messages (
  id TEXT PRIMARY KEY,
  subject TEXT,
  from_addr TEXT,
  received_at DATETIME,
  importance TEXT,
  is_read INTEGER,
  category TEXT,
  tags TEXT,
  archived_at DATETIME,
  raw_link TEXT
);

-- Folder sync state
CREATE TABLE folder_sync (
  folder_id TEXT PRIMARY KEY,
  delta_token TEXT,
  last_synced_at DATETIME
);

-- Digest deliveries
CREATE TABLE deliveries (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  window_start DATETIME,
  window_end DATETIME,
  created_at DATETIME
);
```

## Categories
1. People
2. Clients / Leads
3. Finance / Receipts
4. Auth / Security
5. Marketing / Newsletters
6. Product Updates
7. Ops / Alerts
8. Community / Social
9. Projects
10. Later / Low-priority

## Classification Process
1. **Rules layer**: Keyword/header-based (e.g., `List-Unsubscribe` → Newsletters)
2. **Embeddings layer**: `text-embedding-3-small` for semantic matching
3. **LLM adjudicator**: Claude/GPT for final category + tags
4. **Confidence threshold**: <0.6 → "Needs Review" (don't archive)

## Auto-Archive Categories
- Marketing / Newsletters
- Auth / Security
- Finance / Receipts
- Product Updates

**Never archive:**
- Company domain emails
- Flagged/high-importance
- Recently replied threads

## Config (`~/.automatr/config.yaml`)

```yaml
client_id: "<client-id>"
tenant_id: "<tenant-id>"
redirect_uri: "http://localhost:8383/callback"
scopes:
  - Mail.ReadWrite
  - MailboxSettings.ReadWrite
  - Mail.Send
  - offline_access
summary_schedule:
  - "08:30"
  - "16:30"
timezone: "Europe/Copenhagen"
db_path: "~/.automatr/automatr.db"
```

## Runtime Flow
1. Authenticate → ensure categories exist → resolve Archive folder ID
2. Delta polling: `/me/mailFolders/inbox/messages/delta`
3. For each new message: classify → apply category → (optionally) archive
4. Twice-daily: generate & deliver digest

## Performance Requirements
- Process: <2s/message average
- Storage: <200MB total
- Idempotent delta sync (no duplicates)

## Development Phases

### Phase 1: Foundation (Current)
- [ ] Project scaffold
- [ ] OAuth2 PKCE auth working
- [ ] Read-only Outlook connection
- [ ] List and read emails (non-destructive)

### Phase 2: Classification (Code only)
- [ ] Implement classification logic (don't execute)
- [ ] Rules engine
- [ ] Embeddings integration
- [ ] LLM adjudicator

### Phase 3: Production (With permission)
- [ ] Test classification on specific emails
- [ ] Archive integration
- [ ] Digest generation
- [ ] Scheduling

## Outlook Message Links
`https://outlook.office.com/mail/id/{message_id}`
