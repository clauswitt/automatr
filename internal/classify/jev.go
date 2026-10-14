package classify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

const jevEndpoint = "https://api.typesafe.ai/v1/systemone"

// JevClassifier picks a category with TypeSafe's Jev System One model.
// Unlike a generative LLM it returns a calibrated probability per category,
// so the confidence it reports can be used directly for archive / review gates.
type JevClassifier struct {
	apiKey string
	model  string
	client *http.Client
}

func NewJevClassifier(apiKey, model string) *JevClassifier {
	if model == "" {
		model = "jev-latest"
	}
	return &JevClassifier{
		apiKey: apiKey,
		model:  model,
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

type jevQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria,omitempty"`
}

// Triage holds Jev's probabilities for the archive decision.
type Triage struct {
	NeedsMe      float64 // P(the mailbox owner must personally read/reply/act)
	TimeBound    float64 // P(one-time code / magic link / reset that expires soon)
	ColdOutreach float64 // P(unsolicited pitch from a stranger selling to the owner)
	Model        string
}

var triageQuestions = map[string]jevQuestion{
	"needs_me": {
		Type: "noul",
		Instructions: "Would the recipient (the mailbox owner) miss something important if this email were filed away unread? " +
			"Answer yes only if this specific email needs them to personally reply, decide, approve, pay or fix something, " +
			"or a real person is writing to them directly.",
		Criteria: map[string]string{
			"true": "A real person writing directly to the recipient; a question or request addressed to them personally; " +
				"a failed or overdue payment; a security problem with their own account; a production incident they must fix.",
			"false": "Automated notifications and digests (e.g. 'you have N unread notifications', something was approved, " +
				"completed or updated), reminders pointing to another app where the work happens, newsletters, marketing and " +
				"win-back offers, receipts, credit or plan updates, product updates and other FYI mail.",
		},
	},
	"cold_outreach": {
		Type: "noul",
		Instructions: "Is this unsolicited cold outreach where a stranger is trying to sell something TO the recipient: " +
			"their own product, service, agency work, funding or investment offer, paid media feature, or a meeting to pitch it?",
		Criteria: map[string]string{
			"true": "Mass or templated pitches from people or companies with no existing relationship, including fake 'Re:' " +
				"threads, 'quick follow-up' chasers, flattering 'question about your company' openers, investor or capital " +
				"pitches, and paid PR or feature offers.",
			"false": "Someone who wants to buy from, hire, or become a customer of the recipient's company (inbound leads, " +
				"demo or pricing requests); introductions referred by a named mutual contact; partnership proposals that name " +
				"a mutual contact; replies in real conversations; existing clients, colleagues, support tickets; automated " +
				"notifications and receipts.",
		},
	},
	"time_bound": {
		Type: "noul",
		Instructions: "Is this a short-lived authentication email: a one-time code, verification code, " +
			"magic sign-in link, login approval or password reset link that expires soon?",
		Criteria: map[string]string{
			"true":  "Contains a one-time code or link the recipient must use within minutes or hours.",
			"false": "Anything else, including general security notices, new-login alerts and account changes.",
		},
	},
}

type jevRequest struct {
	State     map[string]string      `json:"state"`
	Model     string                 `json:"model"`
	Questions map[string]jevQuestion `json:"questions"`
}

type jevResponse struct {
	Model   string `json:"model"`
	Answers map[string]struct {
		Type          string             `json:"type"`
		Choice        string             `json:"choice"`
		Probabilities map[string]float64 `json:"probabilities"`
		Confidence    float64            `json:"confidence"`
		Noul          *float64           `json:"noul"`
	} `json:"answers"`
}

// Classify asks Jev to pick one of the given categories (name -> description)
// and, in the same call, answers the triage questions.
func (j *JevClassifier) Classify(ctx context.Context, email EmailInput, categories map[string]string) (*Classification, *Triage, error) {
	if len(categories) == 0 {
		return nil, nil, fmt.Errorf("jev: no categories to choose from")
	}

	questions := map[string]jevQuestion{
		"category": {
			Type: "choice",
			Instructions: "Which inbox category does this email belong to? " +
				"Mail from a real person writing directly to the recipient is People or Clients, " +
				"even if it mentions a product. Bulk, automated or no-reply mail is never People.",
			Criteria: categories,
		},
	}
	for id, q := range triageQuestions {
		questions[id] = q
	}

	result, err := j.evaluate(ctx, email, questions)
	if err != nil {
		return nil, nil, err
	}

	answer, ok := result.Answers["category"]
	if !ok || answer.Choice == "" {
		return nil, nil, fmt.Errorf("jev: no category answer in response")
	}

	classification := &Classification{
		Category:   answer.Choice,
		Confidence: answer.Confidence,
		Reasoning:  fmt.Sprintf("%s: %s", result.Model, topProbabilities(answer.Probabilities, 3)),
	}
	triage, err := triageFrom(result)
	if err != nil {
		return nil, nil, err
	}
	return classification, triage, nil
}

// Triage answers only the archive questions, for emails whose category came
// from rules or learned rules.
func (j *JevClassifier) Triage(ctx context.Context, email EmailInput) (*Triage, error) {
	result, err := j.evaluate(ctx, email, triageQuestions)
	if err != nil {
		return nil, err
	}
	return triageFrom(result)
}

func triageFrom(result *jevResponse) (*Triage, error) {
	t := &Triage{Model: result.Model}
	for id, dst := range map[string]*float64{
		"needs_me":      &t.NeedsMe,
		"time_bound":    &t.TimeBound,
		"cold_outreach": &t.ColdOutreach,
	} {
		answer, ok := result.Answers[id]
		if !ok || answer.Noul == nil {
			return nil, fmt.Errorf("jev: missing %s answer in response", id)
		}
		*dst = *answer.Noul
	}
	return t, nil
}

func (j *JevClassifier) evaluate(ctx context.Context, email EmailInput, questions map[string]jevQuestion) (*jevResponse, error) {
	reqBody := jevRequest{
		State: map[string]string{
			"from":    email.From,
			"subject": email.Subject,
			"body":    truncate(email.Body, 4000),
		},
		Model:     j.model,
		Questions: questions,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", jevEndpoint, bytes.NewReader(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+j.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := j.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Jev API error %d: %s", resp.StatusCode, string(body))
	}

	var result jevResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// topProbabilities formats the n most likely options, e.g. "Marketing 0.91, Product Updates 0.06".
func topProbabilities(probs map[string]float64, n int) string {
	names := make([]string, 0, len(probs))
	for name := range probs {
		names = append(names, name)
	}
	sort.Slice(names, func(a, b int) bool { return probs[names[a]] > probs[names[b]] })
	if len(names) > n {
		names = names[:n]
	}
	parts := make([]string, len(names))
	for i, name := range names {
		parts[i] = fmt.Sprintf("%s %.2f", name, probs[name])
	}
	return strings.Join(parts, ", ")
}
