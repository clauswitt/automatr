# **Automatr – Product Requirements Document (PRD)**

**Version:** 0.1\
**Date:** 2025-10-06

---

## 1. Overview

**Automatr** is a local-first, privacy-preserving assistant that connects to Microsoft Outlook (work account) via Microsoft Graph.\
Its purpose is to **intelligently categorize, label, archive, and summarize incoming emails**, delivering a concise twice-daily digest of your inbox.

It is built in **Go 1.24.0 (darwin/arm64)** and designed to run entirely on the user’s machine — no external backend services or cloud dependencies beyond the Outlook Graph API and LLM endpoints (Anthropic & OpenAI).

---

## 2. Goals & Non-Goals

### 🌟 Goals

- Connect to Outlook (Microsoft 365 work account) via **OAuth2 PKCE** and Microsoft Graph.
- Fetch new messages using **delta sync**, avoiding redundant fetches.
- Classify and tag emails into human-defined categories.
- Apply **Outlook Categories** and move non-urgent messages to **Archive** automatically.
- Generate **twice-daily digests** (HTML + plaintext), summarizing new messages by category.
- Send the digest to yourself via email (optional).
- Store everything locally using **SQLite** and simple YAML config.
- Provide transparency — you can inspect every decision Automatr makes.

### 🚫 Non-Goals

- No AI-generated reply drafts or autoresponders.
- No Gmail or IMAP support (initial release = Outlook only).
- No central server, cloud sync, or multi-device state.
- No web interface (initially).

---

## 3. User Stories

| # | As a... | I want...                                                       | So that...                             |
| - | ------- | --------------------------------------------------------------- | -------------------------------------- |
| 1 | User    | Automatr to read my Outlook inbox and tag emails by category    | I can find important messages faster   |
| 2 | User    | Automatic archiving of newsletters, receipts, and notifications | My inbox stays clean                   |
| 3 | User    | Twice-daily summaries grouped by category                       | I can catch up in a few minutes        |
| 4 | User    | Everything to run locally                                       | No corporate IT or data privacy issues |
| 5 | User    | To edit categories or whitelists                                | I can fine-tune how emails are handled |
| 6 | User    | To inspect logs and decisions                                   | I can trust Automatr’s automation      |

---

## 4. Functional Requirements

### 4.1 Authentication

- Use OAuth2 Authorization Code + PKCE.
- Redirect URI: `http://localhost:8383/callback`.
- Required scopes:
  - `Mail.ReadWrite`
  - `MailboxSettings.ReadWrite`
  - `Mail.Send`
  - `offline_access`
- Tokens stored in: `~/.automatr/token.json`.

### 4.2 Config

YAML config located at `~/.automatr/config.yaml`:

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

### 4.3 Database

SQLite schema (simplified):

```sql
CREATE TABLE IF NOT EXISTS state (
  key TEXT PRIMARY KEY,
  value TEXT
);
CREATE TABLE IF NOT EXISTS messages (
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
CREATE TABLE IF NOT EXISTS folder_sync (
  folder_id TEXT PRIMARY KEY,
  delta_token TEXT,
  last_synced_at DATETIME
);
CREATE TABLE IF NOT EXISTS deliveries (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  window_start DATETIME,
  window_end DATETIME,
  created_at DATETIME
);
```

---

## 5. Categorization & Rules

### Categories

- **People**
- **Clients / Leads**
- **Finance / Receipts**
- **Auth / Security**
- **Marketing / Newsletters**
- **Product Updates**
- **Ops / Alerts**
- **Community / Social**
- **Projects**
- **Later / Low-priority**

### Classification Process

1. **Rules layer:** Keyword and header-based (e.g., `List-Unsubscribe` → Newsletters).
2. **Embeddings layer:** `text-embedding-3-small` used to find semantic neighbors.
3. **LLM adjudicator:** Claude or GPT model verifies and assigns final category + tags.
4. **Confidence threshold:** <0.6 → do not archive (mark “Needs Review”).

### Archiving Logic

- Automatically archive messages categorized as:
  - Marketing / Newsletters
  - Auth / Security
  - Receipts / Finance
  - Product Updates
- Never archive:
  - Messages from company domain(s)
  - Flagged or high-importance messages
  - Threads the user replied to recently

---

## 6. Summaries

### Timing

- Twice daily (default: 08:30 & 16:30 CET).

### Format

**Subject:**\
`[Automatr Digest] {YYYY-MM-DD HH:MM}`

**Body:**\
Per-category sections, including:

- Category title + total count
- 3–7 short bullets (subject lines, top senders)
- Optional 🔧 action callouts
- Links to messages (`https://outlook.office.com/mail/id/{id}`)

Stored at:\
`~/.automatr/summaries/{YYYY-MM-DD-HHMM}.html`

Optional email delivery via Graph `/me/sendMail`.

---

## 7. Architecture

### Components

| Component              | Role                                 |
| ---------------------- | ------------------------------------ |
| `internal/auth`        | PKCE OAuth2 flow + token persistence |
| `internal/graph`       | Thin Microsoft Graph wrapper         |
| `internal/classify`    | Rules + embeddings + LLM adjudicator |
| `internal/store`       | SQLite access + migrations           |
| `internal/summary`     | Digest generation (HTML + plaintext) |
| `internal/scheduler`   | Cron-based job runner                |
| `cmd/automatr/main.go` | Main entry point, orchestrator       |

### Runtime Flow

1. On startup: authenticate → ensure categories exist → resolve Archive folder ID.
2. Start delta polling loop (`/me/mailFolders/inbox/messages/delta`).
3. For each new message:
   - Classify → apply category → move to archive (if applicable).
   - Persist metadata locally.
4. Twice-daily cron jobs: generate & deliver digest.

---

## 8. Non-Functional Requirements

| Aspect            | Requirement                                  |
| ----------------- | -------------------------------------------- |
| **Performance**   | Process new mail in <2 s/message on average  |
| **Reliability**   | Idempotent delta sync; no duplicates         |
| **Storage**       | <200 MB total (SQLite + embeddings cache)    |
| **Security**      | No secrets stored; PKCE tokens only          |
| **Privacy**       | No third-party storage of message data       |
| **Compatibility** | macOS (Apple Silicon, Go 1.24.0)             |
| **Upgradability** | Modular design; easy to add IMAP/Gmail later |

---

## 9. Future Enhancements (v2+)

- Support multiple Outlook accounts.
- Optional Slack/Telegram notification channel.
- Smart “attention” scoring per message.
- Local web dashboard for rule tuning.
- Integration with macOS notifications.
- CLI query interface (`automatr summary today`, `automatr classify --dry-run`).

---

## 10. Milestones

| Week | Deliverable                              |
| ---- | ---------------------------------------- |
| 1    | Project scaffolding + PKCE auth working  |
| 2    | Delta sync + message persistence         |
| 3    | Classification (rules + embeddings)      |
| 4    | Outlook categories & archive integration |
| 5    | Digest generator + local output          |
| 6    | Summary emailing + polish                |
| 7    | Optional CLI commands, packaging         |

---

## 11. Success Criteria

- Fully functional local agent that classifies and summarizes Outlook mail with no server component.
- End-to-end runs twice a day, automatically producing digests.
- Stable for daily use on macOS.
- Easy to inspect and extend.

