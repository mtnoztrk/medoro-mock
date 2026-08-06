package main

// medoro-mock — a stand-in for the Medoro (ipsp.lv) payment gateway, for UAT only.
//
// Flow:
//  1. The backend's auto-submit form POSTs INTERFACE/KEY/DATA/SIGNATURE/CALLBACK to
//     /form/v2/ exactly as it would to https://ipsp.lv/form/v2/.
//  2. The mock decrypts DATA with the gateway private key, shows a payment page with the
//     order summary and the scenario list from scenarios.json.
//  3. The tester picks a scenario. The mock renders the callback XML, encrypts and signs
//     it, waits ~1.5 s, then the browser form-POSTs it to the CALLBACK url. The backend
//     processes it and redirects the browser back to the site — same as production.
//     Optionally the mock also fires the server-to-server callback from its own backend.
//
// Zero configuration: fixed port 2727, the two test PEM files next to the binary (the same
// files the backend uses — keep them byte-identical), scenarios in scenarios.json.

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*.html
var templateFS embed.FS

var pages = template.Must(template.ParseFS(templateFS, "templates/*.html"))

const (
	listenAddr    = ":2727"
	scenariosPath = "scenarios.json"
	gatewayKey    = "medoro_gateway.pem"
	merchantKey   = "medoro_merchant.pem"
	callbackDelay = 1500 * time.Millisecond
	sessionTTL    = 2 * time.Hour
)

// medoroRequest mirrors the fields of MedoroPaymentRequest the mock cares about.
type medoroRequest struct {
	XMLName xml.Name `xml:"data"`
	Order   struct {
		ID          string `xml:"ID"`
		Amount      int64  `xml:"Amount"`
		Currency    string `xml:"Currency"`
		Description string `xml:"Description"`
	} `xml:"Order"`
	BillingAddress struct {
		Name  string `xml:"Name"`
		Email string `xml:"Email"`
	} `xml:"BillingAddress"`
}

type paymentSession struct {
	ID          string
	OrderID     string
	Amount      int64
	Currency    string
	Description string
	Name        string
	Email       string
	CallbackURL string
	CreatedAt   time.Time
}

type server struct {
	keys *keyring

	mu       sync.Mutex
	sessions map[string]*paymentSession
}

func main() {
	keys, err := loadKeys(gatewayKey, merchantKey)
	if err != nil {
		log.Fatalf("cannot load keys: %v", err)
	}
	if _, err := loadScenarios(scenariosPath); err != nil {
		log.Fatalf("cannot load scenarios: %v", err)
	}

	s := &server{keys: keys, sessions: map[string]*paymentSession{}}
	go s.cleanupLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.handleIndex)
	mux.HandleFunc("POST /form/v2/", s.handleForm)
	mux.HandleFunc("POST /form/v2", s.handleForm)
	mux.HandleFunc("POST /manual", s.handleManual)
	mux.HandleFunc("GET /pay", s.handlePay)
	mux.HandleFunc("POST /confirm", s.handleConfirm)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	log.Printf("medoro-mock listening on %s", listenAddr)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}

// handleIndex shows the manual-start form plus the loaded scenario list.
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	scenarios, err := loadScenarios(scenariosPath)
	if err != nil {
		s.errorPage(w, "Cannot load scenarios", err.Error())
		return
	}
	s.render(w, "index.html", map[string]any{"Scenarios": scenarios})
}

// handleForm is the drop-in replacement for https://ipsp.lv/form/v2/.
func (s *server) handleForm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, "Bad form post", err.Error())
		return
	}
	data := r.PostFormValue("DATA")
	key := r.PostFormValue("KEY")
	sig := r.PostFormValue("SIGNATURE")
	callback := r.PostFormValue("CALLBACK")
	if data == "" || key == "" || callback == "" {
		s.errorPage(w, "Missing fields", "The form post must contain DATA, KEY and CALLBACK — is the backend pointing its Medoro ApiUrl at this mock?")
		return
	}

	xmlData, err := s.keys.decryptRequest(data, key)
	if err != nil {
		s.errorPage(w, "Cannot decrypt DATA", err.Error())
		return
	}
	if sig != "" {
		if err := s.keys.verifyRequestSignature(xmlData, sig); err != nil {
			// The flow still works; log it so a key mismatch is visible.
			log.Printf("WARNING: request signature does not verify against keys/mock/merchant_public.pem: %v", err)
		}
	}

	var req medoroRequest
	if err := xml.Unmarshal(xmlData, &req); err != nil {
		s.errorPage(w, "Cannot parse payment XML", err.Error())
		return
	}
	log.Printf("payment request: order=%s amount=%d %s callback=%s", req.Order.ID, req.Order.Amount, req.Order.Currency, callback)

	sess := &paymentSession{
		ID:          randomID(),
		OrderID:     req.Order.ID,
		Amount:      req.Order.Amount,
		Currency:    req.Order.Currency,
		Description: req.Order.Description,
		Name:        strings.TrimSpace(req.BillingAddress.Name),
		Email:       req.BillingAddress.Email,
		CallbackURL: callback,
		CreatedAt:   time.Now(),
	}
	s.putSession(sess)
	http.Redirect(w, r, "/pay?id="+sess.ID, http.StatusSeeOther)
}

// handleManual starts a session without an encrypted request, for firing callbacks by hand.
func (s *server) handleManual(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, "Bad form post", err.Error())
		return
	}
	amount, err := strconv.ParseInt(strings.TrimSpace(r.PostFormValue("amount")), 10, 64)
	if err != nil || amount <= 0 {
		s.errorPage(w, "Invalid amount", "Amount must be a positive whole number of minor units (cents).")
		return
	}
	callback := strings.TrimSpace(r.PostFormValue("callback"))
	if _, err := url.ParseRequestURI(callback); err != nil {
		s.errorPage(w, "Invalid callback URL", "Enter the full URL of the medoro-callback endpoint?.")
		return
	}
	orderID := strings.TrimSpace(r.PostFormValue("orderId"))
	if orderID == "" {
		s.errorPage(w, "Missing order ID", "Enter the payment transaction reference (GUID).")
		return
	}
	sess := &paymentSession{
		ID:          randomID(),
		OrderID:     orderID,
		Amount:      amount,
		Currency:    "USD",
		Description: strings.TrimSpace(r.PostFormValue("description")),
		Name:        "Manual Tester",
		Email:       "uat-tester@example.com",
		CallbackURL: callback,
		CreatedAt:   time.Now(),
	}
	s.putSession(sess)
	http.Redirect(w, r, "/pay?id="+sess.ID, http.StatusSeeOther)
}

// handlePay renders the payment page: order summary plus scenario selection.
func (s *server) handlePay(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.getSession(r.URL.Query().Get("id"))
	if !ok {
		s.errorPage(w, "Payment not found", "The payment session expired or the id is wrong. Start the purchase again.")
		return
	}
	scenarios, err := loadScenarios(scenariosPath)
	if err != nil {
		s.errorPage(w, "Cannot load scenarios", err.Error())
		return
	}
	s.render(w, "pay.html", map[string]any{
		"Session":       sess,
		"Scenarios":     scenarios,
		"AmountDisplay": fmt.Sprintf("%.2f", float64(sess.Amount)/100),
	})
}

// handleConfirm builds the encrypted callback for the chosen scenario and hands it to the
// browser, which auto-posts it to the backend's callback endpoint after a short delay.
func (s *server) handleConfirm(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, "Bad form post", err.Error())
		return
	}
	sess, ok := s.getSession(r.PostFormValue("id"))
	if !ok {
		s.errorPage(w, "Payment not found", "The payment session expired. Start the purchase again.")
		return
	}
	scenarios, err := loadScenarios(scenariosPath)
	if err != nil {
		s.errorPage(w, "Cannot load scenarios", err.Error())
		return
	}
	scenario, ok := findScenario(scenarios, r.PostFormValue("scenario"))
	if !ok {
		s.errorPage(w, "Unknown scenario", "The selected scenario no longer exists in scenarios.json.")
		return
	}

	xmlStr, err := renderScenario(scenario, sess)
	if err != nil {
		s.errorPage(w, "Scenario template error", err.Error())
		return
	}
	dataB64, keyB64, sigB64, err := s.keys.encryptCallback([]byte(xmlStr))
	if err != nil {
		s.errorPage(w, "Cannot encrypt callback", err.Error())
		return
	}
	log.Printf("callback: order=%s scenario=%s -> %s", sess.OrderID, scenario.ID, sess.CallbackURL)

	// Optional server-to-server callback, like Medoro's own S2S notification. The backend
	// locks the payment row and ignores repeats, so double delivery is safe.
	if r.PostFormValue("s2s") == "on" {
		serverURL := strings.Replace(sess.CallbackURL, "medoro-callback", "medoro-server-callback", 1)
		go func() {
			time.Sleep(callbackDelay)
			resp, err := http.PostForm(serverURL, url.Values{
				"DATA":      {dataB64},
				"KEY":       {keyB64},
				"SIGNATURE": {sigB64},
			})
			if err != nil {
				log.Printf("S2S callback to %s failed: %v", serverURL, err)
				return
			}
			resp.Body.Close()
			log.Printf("S2S callback to %s -> %s", serverURL, resp.Status)
		}()
	}
	s.deleteSession(sess.ID)

	s.render(w, "redirect.html", map[string]any{
		"CallbackURL":   sess.CallbackURL,
		"Data":          dataB64,
		"Key":           keyB64,
		"Signature":     sigB64,
		"ScenarioLabel": scenario.Label,
		"DelayMs":       int(callbackDelay / time.Millisecond),
	})
}

func (s *server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pages.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template %s: %v", name, err)
	}
}

func (s *server) errorPage(w http.ResponseWriter, title, detail string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	if err := pages.ExecuteTemplate(w, "error.html", map[string]any{"Title": title, "Detail": detail}); err != nil {
		log.Printf("template error.html: %v", err)
	}
}

func (s *server) putSession(sess *paymentSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[sess.ID] = sess
}

func (s *server) getSession(id string) (*paymentSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	return sess, ok
}

func (s *server) deleteSession(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

func (s *server) cleanupLoop() {
	for range time.Tick(10 * time.Minute) {
		cutoff := time.Now().Add(-sessionTTL)
		s.mu.Lock()
		for id, sess := range s.sessions {
			if sess.CreatedAt.Before(cutoff) {
				delete(s.sessions, id)
			}
		}
		s.mu.Unlock()
	}
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
