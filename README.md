# Automatr

Local-first email assistant for Microsoft Outlook that intelligently categorizes, archives, and summarizes your inbox.

## Quick Start

1. **Set up Azure App Registration**
   - Go to [Azure Portal](https://portal.azure.com) → Azure Active Directory → App Registrations
   - Create a new registration with redirect URI: `http://localhost:8383/callback`
   - Note your Client ID and Tenant ID

2. **Configure environment**
   ```bash
   export AUTOMATR_CLIENT_ID="your-client-id"
   export AUTOMATR_TENANT_ID="your-tenant-id"
   ```

3. **Build and authenticate**
   ```bash
   go build -o automatr ./cmd/automatr
   ./automatr -auth
   ```

4. **List recent emails**
   ```bash
   ./automatr -list
   ```

5. **Read a specific email**
   ```bash
   ./automatr -read <message-id>
   ```

## Development Status

🚧 **Phase 1: Foundation** (In Progress)
- [x] Project scaffold
- [x] OAuth2 PKCE authentication
- [x] Microsoft Graph client (read-only)
- [x] SQLite store
- [ ] Test with live Outlook data

🔜 **Phase 2: Classification**
- [ ] Classification engine (rules + embeddings + LLM)
- [ ] Category management
- [ ] Archive automation (code only, not tested)

🔜 **Phase 3: Production**
- [ ] Digest generation
- [ ] Email delivery
- [ ] Scheduling

## Architecture

See [CLAUDE.md](./CLAUDE.md) for development context and [docs/prd.md](./docs/prd.md) for full requirements.
