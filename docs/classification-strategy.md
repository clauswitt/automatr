# Classification Strategy

## Overview

Three-layer classification system to categorize emails efficiently while minimizing API costs.

## Categories

1. **People** - Direct personal communication
2. **Clients / Leads** - Business opportunities, customer emails
3. **Finance / Receipts** - Invoices, payments, financial statements
4. **Auth / Security** - Login codes, security alerts, password resets
5. **Marketing / Newsletters** - Promotional emails, newsletters
6. **Product Updates** - Release notes, product announcements
7. **Ops / Alerts** - System alerts, monitoring notifications
8. **Community / Social** - Forums, social network notifications
9. **Projects** - Project-specific communications
10. **Later / Low-priority** - Everything else

## Three-Layer Classification

### Layer 1: Rule-Based (Free, Fast)

**Headers & Metadata:**
- `List-Unsubscribe` header → Marketing/Newsletters
- `X-Auto-Response-Suppress` → Alerts/Automated
- High `Importance` flag → Check manually
- Multiple recipients (20+) → Marketing

**Sender Patterns:**
- `noreply@`, `no-reply@` → Automated
- `alert@`, `monitoring@` → Ops/Alerts
- `invoice@`, `billing@` → Finance
- `security@`, `2fa@` → Auth/Security
- Known domains (Stripe, AWS, GitHub, etc.) → Specific categories

**Subject Keywords:**
```yaml
Auth/Security:
  - "verification code"
  - "password reset"
  - "2FA"
  - "security alert"
  - "login attempt"

Finance:
  - "invoice"
  - "receipt"
  - "payment"
  - "billing"
  - "subscription renewed"

Ops/Alerts:
  - "alert:"
  - "triggered:"
  - "recovered:"
  - "down:"
  - "error rate"

Marketing:
  - "unsubscribe"
  - "view in browser"
  - "special offer"
  - "limited time"
```

**Confidence:** If rule matches → 0.9 confidence

### Layer 2: Embeddings (Cheap, Semantic)

**When to use:** Rule didn't match or confidence < 0.8

**Process:**
1. Extract email text (subject + first 500 chars of body)
2. Generate embedding via `text-embedding-3-small` ($0.02 / 1M tokens)
3. Compare with cached category embeddings (cosine similarity)
4. Top match with similarity > 0.7 → assign category

**Category Examples (for embedding):**
```
People: "Hey, can we catch up tomorrow? Let me know when works."
Clients/Leads: "Interested in your services. Could we schedule a demo?"
Finance: "Your monthly invoice for $99 is attached."
Auth: "Your verification code is 123456"
Marketing: "This week only: 50% off all items! Shop now."
Product Updates: "Version 2.0 released with new features."
Ops/Alerts: "Server CPU usage exceeded 90% threshold."
```

**Cost:** ~$0.00002 per email (500 tokens @ $0.02/1M)

**Confidence:** Similarity score (0.0-1.0)

### Layer 3: LLM Adjudicator (Expensive, Accurate)

**When to use:**
- No rule match AND embedding confidence < 0.7
- OR user requests re-classification
- OR email flagged for review

**Model options:**
- **Claude Haiku** ($0.25/$1.25 per 1M tokens) - Recommended for speed/cost
- **GPT-4o-mini** ($0.15/$0.60 per 1M tokens) - Alternative

**Prompt:**
```
Classify this email into ONE category:

Categories:
- People: Direct personal communication
- Clients/Leads: Business opportunities
- Finance: Invoices, payments, receipts
- Auth/Security: Login codes, 2FA, security alerts
- Marketing: Newsletters, promotions
- Product Updates: Release notes, announcements
- Ops/Alerts: System monitoring, alerts
- Community: Forums, social notifications
- Projects: Project-specific work
- Later: Low priority, misc

Email:
From: {from}
Subject: {subject}
Body: {first 1000 chars}

Return JSON:
{
  "category": "category_name",
  "confidence": 0.0-1.0,
  "reasoning": "brief explanation"
}
```

**Cost:** ~$0.0003 per email (1000 input + 100 output tokens)

**Confidence:** Model-provided (0.0-1.0)

## Decision Flow

```
Email arrives
    ↓
Rule match? → YES → confidence 0.9 → [Assign Category]
    ↓ NO
Generate embedding (cache locally)
    ↓
Similarity > 0.7? → YES → [Assign Category]
    ↓ NO
LLM adjudication
    ↓
Confidence > 0.6? → YES → [Assign Category]
    ↓ NO
Mark as "Needs Review"
```

## Cost Optimization

**Assumptions:**
- 100 emails/day
- 70% caught by rules (free)
- 20% need embeddings ($0.002/day)
- 10% need LLM ($0.03/day)

**Total:** ~$1/month for 3000 emails

**Optimizations:**
1. **Cache embeddings** - Store in SQLite, reuse for similar emails
2. **Batch processing** - Process multiple emails in one LLM call
3. **Smart sampling** - Only re-classify if sender/pattern changes
4. **Learning** - User corrections update rules

## Auto-Archive Logic

Only archive if:
- ✅ Confidence > 0.8
- ✅ Category in: [Marketing, Auth, Finance, Product Updates]
- ✅ NOT from company domain (@company.com)
- ✅ NOT flagged/high-importance
- ✅ NOT part of recent thread user replied to

Otherwise: Apply category, keep in Inbox

## Testing Strategy

**Phase 1: Non-destructive**
```bash
./automatr classify -dry-run -limit 10
```
Shows classifications without applying

**Phase 2: Selective testing**
```bash
./automatr classify -id <message-id>
```
Classify and apply category to specific email

**Phase 3: Batch with review**
```bash
./automatr classify -batch 50 -no-archive
```
Apply categories, but don't move to archive yet

**Phase 4: Full automation**
```bash
./automatr classify -auto
```
Run with archive enabled

## Implementation Plan

1. ✅ Create classification engine structure
2. ✅ Implement rule-based classifier
3. ✅ Implement embeddings classifier (with local cache)
4. ✅ Implement LLM adjudicator
5. ✅ Add dry-run testing mode
6. ✅ Add Outlook category management
7. ✅ Add archive logic (disabled by default)

## Questions to Decide

1. **LLM Provider:** Claude Haiku vs GPT-4o-mini vs both?
2. **Embedding cache:** How long to keep? (Suggest: indefinite, it's tiny)
3. **Re-classification:** Auto-update categories when rules improve?
4. **Custom categories:** Allow user-defined categories beyond the 10?
5. **Learning:** Should user corrections automatically update rules?
