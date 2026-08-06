package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"text/template"
	"time"
)

// Scenario is one selectable outcome on the payment page. The XML field is a Go
// text/template; the placeholders come from callbackData below. Add more scenarios by
// appending entries to scenarios.json — no rebuild needed, the file is read per request
// when running from the repo, and can be volume-mounted over the baked-in copy in Docker.
type Scenario struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Kind        string `json:"kind"` // "success" or "failure", only used for page styling
	Description string `json:"description"`
	XML         string `json:"xml"`
}

type scenarioFile struct {
	Scenarios []Scenario `json:"scenarios"`
}

// callbackData holds every placeholder a scenario template may use.
type callbackData struct {
	PaymentID   string
	OrderID     string
	Amount      int64
	Currency    string
	Description string
	Name        string
	Email       string
	StartDate   string
	LastDate    string
}

func loadScenarios(path string) ([]Scenario, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f scenarioFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(f.Scenarios) == 0 {
		return nil, fmt.Errorf("%s contains no scenarios", path)
	}
	return f.Scenarios, nil
}

func findScenario(scenarios []Scenario, id string) (Scenario, bool) {
	for _, s := range scenarios {
		if s.ID == id {
			return s, true
		}
	}
	return Scenario{}, false
}

// renderScenario fills the scenario template with values from the payment session.
func renderScenario(s Scenario, sess *paymentSession) (string, error) {
	tmpl, err := template.New(s.ID).Parse(s.XML)
	if err != nil {
		return "", fmt.Errorf("scenario %q has an invalid template: %w", s.ID, err)
	}
	now := time.Now().UTC()
	data := callbackData{
		PaymentID:   fmt.Sprintf("%d", 225000000+rand.Int63n(999999)),
		OrderID:     sess.OrderID,
		Amount:      sess.Amount,
		Currency:    sess.Currency,
		Description: xmlEscape(sess.Description),
		Name:        xmlEscape(sess.Name),
		Email:       xmlEscape(sess.Email),
		StartDate:   now.Add(-30 * time.Second).Format("2006-01-02 15:04:05"),
		LastDate:    now.Format("2006-01-02 15:04:05"),
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("scenario %q failed to render: %w", s.ID, err)
	}
	return b.String(), nil
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}
