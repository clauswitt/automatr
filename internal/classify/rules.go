package classify

import (
	"regexp"
	"strings"
)

// RuleBasedClassifier classifies emails using simple rules
type RuleBasedClassifier struct {
	rules []Rule
}

type Rule struct {
	Name       string
	Category   string
	Confidence float64
	Check      func(email EmailInput) bool
}

type EmailInput struct {
	From        string
	Subject     string
	Body        string
	Headers     map[string]string
	ToRecipients int
}

func NewRuleBasedClassifier() *RuleBasedClassifier {
	return &RuleBasedClassifier{
		rules: buildRules(),
	}
}

// Classify attempts to classify email using rules
func (c *RuleBasedClassifier) Classify(email EmailInput) (*Classification, bool) {
	for _, rule := range c.rules {
		if rule.Check(email) {
			return &Classification{
				Category:   rule.Category,
				Confidence: rule.Confidence,
				Reasoning:  "Matched rule: " + rule.Name,
			}, true
		}
	}
	return nil, false
}

func buildRules() []Rule {
	return []Rule{
		// Marketing / Newsletters (based on headers)
		{
			Name:       "List-Unsubscribe header",
			Category:   "Marketing",
			Confidence: 0.95,
			Check: func(email EmailInput) bool {
				_, hasUnsubscribe := email.Headers["List-Unsubscribe"]
				return hasUnsubscribe
			},
		},

		// Auth / Security
		{
			Name:       "Security/Auth sender patterns",
			Category:   "Auth",
			Confidence: 0.9,
			Check: func(email EmailInput) bool {
				lower := strings.ToLower(email.From)
				patterns := []string{"security@", "2fa@", "noreply@", "no-reply@"}
				for _, p := range patterns {
					if strings.Contains(lower, p) {
						return true
					}
				}
				return false
			},
		},
		{
			Name:       "Auth keywords in subject",
			Category:   "Auth",
			Confidence: 0.92,
			Check: func(email EmailInput) bool {
				lower := strings.ToLower(email.Subject)
				keywords := []string{
					"verification code",
					"verify your",
					"password reset",
					"2fa",
					"two-factor",
					"security code",
					"login code",
					"authentication",
					"confirm your email",
				}
				for _, kw := range keywords {
					if strings.Contains(lower, kw) {
						return true
					}
				}
				return false
			},
		},

		// Finance / Receipts
		{
			Name:       "Finance sender patterns",
			Category:   "Finance",
			Confidence: 0.9,
			Check: func(email EmailInput) bool {
				lower := strings.ToLower(email.From)
				patterns := []string{"invoice@", "billing@", "payments@", "receipts@", "stripe.com", "paypal.com"}
				for _, p := range patterns {
					if strings.Contains(lower, p) {
						return true
					}
				}
				return false
			},
		},
		{
			Name:       "Finance keywords in subject",
			Category:   "Finance",
			Confidence: 0.88,
			Check: func(email EmailInput) bool {
				lower := strings.ToLower(email.Subject)
				keywords := []string{
					"invoice",
					"receipt",
					"payment",
					"billing",
					"subscription renewed",
					"payment received",
					"refund",
					"your order",
				}
				for _, kw := range keywords {
					if strings.Contains(lower, kw) {
						return true
					}
				}
				return false
			},
		},

		// Ops / Alerts
		{
			Name:       "Ops/Alert sender patterns",
			Category:   "Ops",
			Confidence: 0.92,
			Check: func(email EmailInput) bool {
				lower := strings.ToLower(email.From)
				patterns := []string{"alert@", "alerts@", "monitoring@", "noreply@", "notifications@"}
				for _, p := range patterns {
					if strings.Contains(lower, p) {
						return true
					}
				}
				return false
			},
		},
		{
			Name:       "Alert keywords in subject",
			Category:   "Ops",
			Confidence: 0.93,
			Check: func(email EmailInput) bool {
				lower := strings.ToLower(email.Subject)
				// Common alert patterns
				patterns := []string{
					"^alert:",
					"^triggered:",
					"^recovered:",
					"down$",
					"error rate",
					"cpu usage",
					"disk space",
					"threshold exceeded",
					"anomaly detected",
				}
				for _, p := range patterns {
					matched, _ := regexp.MatchString(p, lower)
					if matched {
						return true
					}
				}
				return false
			},
		},

		// Product Updates
		{
			Name:       "Product update keywords",
			Category:   "Product Updates",
			Confidence: 0.85,
			Check: func(email EmailInput) bool {
				lower := strings.ToLower(email.Subject)
				keywords := []string{
					"new version",
					"release notes",
					"changelog",
					"product update",
					"what's new",
					"feature announcement",
					"now available",
				}
				for _, kw := range keywords {
					if strings.Contains(lower, kw) {
						return true
					}
				}
				return false
			},
		},

		// Marketing (mass emails)
		{
			Name:       "Mass email recipients",
			Category:   "Marketing",
			Confidence: 0.85,
			Check: func(email EmailInput) bool {
				return email.ToRecipients > 20
			},
		},
		{
			Name:       "Marketing keywords",
			Category:   "Marketing",
			Confidence: 0.8,
			Check: func(email EmailInput) bool {
				lower := strings.ToLower(email.Subject + " " + email.Body)
				keywords := []string{
					"unsubscribe",
					"special offer",
					"limited time",
					"% off",
					"discount",
					"shop now",
					"view in browser",
					"click here",
				}
				matches := 0
				for _, kw := range keywords {
					if strings.Contains(lower, kw) {
						matches++
					}
				}
				return matches >= 2 // Multiple marketing indicators
			},
		},

		// Community / Social
		{
			Name:       "Community sender patterns",
			Category:   "Community",
			Confidence: 0.87,
			Check: func(email EmailInput) bool {
				lower := strings.ToLower(email.From)
				patterns := []string{
					"notifications@",
					"community@",
					"forum@",
					"github.com",
					"stackoverflow.com",
					"reddit.com",
				}
				for _, p := range patterns {
					if strings.Contains(lower, p) {
						return true
					}
				}
				return false
			},
		},
	}
}
