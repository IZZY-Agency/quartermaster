package typesafe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const testKey = "sk-test-secret-123"

const reply = `{"model":"jev-1.14.0","answers":{
"role":{"type":"choice","choice":"review","probabilities":{"review":0.8,"build":0.2}},
"security":{"type":"noul","noul":0.04},
"scope":{"type":"score","score":1.62}}}`

func questions() map[string]Question {
	return map[string]Question{
		"role":     {Type: "choice", Instructions: "Which role?", Criteria: map[string]string{"review": "r", "build": "b"}},
		"security": {Type: "noul", Instructions: "Security?", Criteria: map[string]string{"true": "t", "false": "f"}},
		"scope":    {Type: "score", Instructions: "Scope?", Criteria: map[string]string{"low": "l", "high": "h"}},
	}
}

func newTest(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New(testKey)
	c.BaseURL = srv.URL
	return c
}

func TestAskParsesAnswers(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(reply)) })
	got, err := c.Ask(context.Background(), map[string]string{"brief": "x"}, questions())
	if err != nil {
		t.Fatal(err)
	}
	if a := got["role"]; a.Type != "choice" || a.Choice != "review" || a.Probabilities["review"] != 0.8 {
		t.Errorf("role = %+v", a)
	}
	if a := got["security"]; a.Noul != 0.04 {
		t.Errorf("security = %+v", a)
	}
	if a := got["scope"]; a.Score != 1.62 {
		t.Errorf("scope = %+v", a)
	}
}

func TestAskSendsBearerAndModel(t *testing.T) {
	var auth, path string
	var body map[string]any
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		auth, path = r.Header.Get("Authorization"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(reply))
	})
	if _, err := c.Ask(context.Background(), map[string]string{"brief": "x"}, questions()); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer "+testKey {
		t.Errorf("Authorization = %q", auth)
	}
	if path != "/v1/systemone" {
		t.Errorf("path = %q", path)
	}
	if body["model"] != "jev-1.14.0" {
		t.Errorf("model = %v", body["model"])
	}
	qs, _ := body["questions"].(map[string]any)
	sec, _ := qs["security"].(map[string]any)
	if sec["type"] != "noul" || sec["instructions"] != "Security?" || sec["criteria"] == nil {
		t.Errorf("security question = %v", sec)
	}
	if body["state"] == nil {
		t.Error("no state in request")
	}
}

func TestAsk401IsRejected(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	_, err := c.Ask(context.Background(), "s", questions())
	if !errors.Is(err, ErrRejected) {
		t.Fatalf("err = %v, want ErrRejected", err)
	}
}

func TestAsk500IsError(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	_, err := c.Ask(context.Background(), "s", questions())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 500 {
		t.Fatalf("err = %v, want *APIError 500", err)
	}
}

func TestAskMissingAnswerIsError(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"answers":{"security":{"type":"noul","noul":0.1}}}`))
	})
	if _, err := c.Ask(context.Background(), "s", questions()); err == nil {
		t.Fatal("want error for a missing answer")
	}
}

func TestErrorNeverContainsKey(t *testing.T) {
	long := strings.Repeat("x", 500)
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte("{\"detail\":\"bad key " + testKey + " " + long + "\"}"))
	})
	_, err := c.Ask(context.Background(), "s", questions())
	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), testKey) || strings.Contains(ae.Detail, testKey) {
		t.Errorf("error holds the key: %v", err)
	}
	if n := len([]rune(ae.Detail)); n > 200 {
		t.Errorf("detail has %d runes", n)
	}
	// a transport failure does not leak the key either
	c.BaseURL = "http://127.0.0.1:1/" + testKey
	_, err = c.Ask(context.Background(), "s", questions())
	if err == nil || strings.Contains(err.Error(), testKey) {
		t.Errorf("transport error = %v", err)
	}
}

func TestAskReplyTooLargeIsErrTooLarge(t *testing.T) {
	c := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat(" ", 1<<20+1)))
	})
	if _, err := c.Ask(context.Background(), "s", questions()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestAskBadReplyIsError(t *testing.T) {
	bad := map[string]string{
		"malformed json": `{"answers":`,
		"noul too high":  `{"answers":{"role":{"choice":"review"},"security":{"noul":1.5},"scope":{"score":1}}}`,
		"noul negative":  `{"answers":{"role":{"choice":"review"},"security":{"noul":-0.1},"scope":{"score":1}}}`,
		"noul missing":   `{"answers":{"role":{"choice":"review"},"security":{},"scope":{"score":1}}}`,
		"unknown choice": `{"answers":{"role":{"choice":"deploy"},"security":{"noul":0.1},"scope":{"score":1}}}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			c := newTest(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
			if _, err := c.Ask(context.Background(), "s", questions()); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestErrorNeverContainsEscapedKey(t *testing.T) {
	const key = "sk/key with+chars"
	c := New(key)
	c.BaseURL = "http://127.0.0.1:1/" + url.PathEscape(key)
	_, err := c.Ask(context.Background(), "s", questions())
	if err == nil || strings.Contains(err.Error(), url.PathEscape(key)) || strings.Contains(err.Error(), key) {
		t.Errorf("err = %v", err)
	}
}

func TestNewWithoutKeyIsNil(t *testing.T) {
	if New("") != nil {
		t.Error("New(\"\") is not nil")
	}
	if New(testKey) == nil {
		t.Error("New(key) is nil")
	}
}
