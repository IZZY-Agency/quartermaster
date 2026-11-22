package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/IZZY-Agency/quartermaster/internal/policy"
)

const (
	// DefaultTimeout is the limit for one request.
	DefaultTimeout = 20 * time.Second
	defaultBaseURL = "https://api.typesafe.ai"
	maxBody        = 1 << 20 // reply cap
	detailRunes    = 200
)

// ErrRejected is what Ask returns when TypeSafe answers 401.
var ErrRejected = errors.New("typesafe: key rejected")

// ErrTooLarge is what a reply over 1 MiB is reported as.
var ErrTooLarge = errors.New("typesafe: reply exceeds 1 MiB")

// APIError is a non-200 reply from TypeSafe. Detail is whitespace-collapsed,
// at most 200 runes and never holds the key.
type APIError struct {
	Status int
	Detail string
}

// Error implements error.
func (e *APIError) Error() string {
	d := e.Detail
	if d == "" {
		d = statusText(e.Status)
	}
	return fmt.Sprintf("typesafe: HTTP %d: %s", e.Status, d)
}

// Client talks to TypeSafe. Build it with New.
type Client struct {
	Key     string
	BaseURL string        // "" = https://api.typesafe.ai; tests point it at httptest
	HTTP    *http.Client  // nil = http.DefaultClient
	Timeout time.Duration // 0 = DefaultTimeout
}

// New returns a client for key, or nil when key is empty (Jev is off).
func New(key string) *Client {
	if key == "" {
		return nil
	}
	return &Client{Key: key}
}

type questionBody struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria"`
}

type answerBody struct {
	Noul          *float64           `json:"noul"`
	Choice        string             `json:"choice"`
	Score         *float64           `json:"score"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// Ask sends state and the questions to Jev and returns one Answer per
// question. A 401 is ErrRejected; any other failure, or a reply that lacks an
// answer, is an error, never a "no". One attempt, bounded by Timeout and ctx.
func (c *Client) Ask(ctx context.Context, state any, qs map[string]Question) (map[string]Answer, error) {
	reqQs := make(map[string]questionBody, len(qs))
	for id, q := range qs {
		reqQs[id] = questionBody(q)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]any{"model": policy.Model, "state": state, "questions": reqQs}); err != nil {
		return nil, fmt.Errorf("typesafe: encoding request: %w", err)
	}
	data, err := c.do(ctx, &buf)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			return nil, ErrRejected
		}
		return nil, err
	}
	var rep struct {
		Answers map[string]answerBody `json:"answers"`
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, fmt.Errorf("typesafe: bad reply: %w", err)
	}
	out := make(map[string]Answer, len(qs))
	for id, q := range qs {
		a, ok := rep.Answers[id]
		if !ok {
			return nil, fmt.Errorf("typesafe: reply has no %s answer", id)
		}
		ans := Answer{Type: q.Type, Choice: a.Choice, Probabilities: a.Probabilities}
		switch q.Type {
		case "noul":
			if a.Noul == nil || !(*a.Noul >= 0 && *a.Noul <= 1) {
				return nil, fmt.Errorf("typesafe: %s has no noul in 0..1", id)
			}
			ans.Noul = *a.Noul
		case "score":
			if a.Score == nil {
				return nil, fmt.Errorf("typesafe: %s has no score", id)
			}
			ans.Score = *a.Score
		case "choice":
			if a.Choice == "" {
				return nil, fmt.Errorf("typesafe: %s has no choice", id)
			}
			if !criteriaHas(q.Criteria, a.Choice) {
				return nil, fmt.Errorf("typesafe: %s answered %q, which is not one of its choices", id, a.Choice)
			}
		}
		out[id] = ans
	}
	return out, nil
}

// do POSTs body and returns the body of a 200 reply; any other status is an
// *APIError.
func (c *Client) do(ctx context.Context, body io.Reader) ([]byte, error) {
	t := c.Timeout
	if t == 0 {
		t = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, t)
	defer cancel()
	base := c.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/systemone", body)
	if err != nil {
		return nil, fmt.Errorf("typesafe: %w", c.scrub(err))
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("typesafe: %w", c.scrub(err))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("typesafe: reading reply: %w", c.scrub(err))
	}
	if len(data) > maxBody {
		return nil, ErrTooLarge
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{resp.StatusCode, c.detail(resp.StatusCode, data)}
	}
	return data, nil
}

// scrub hides the key if err's text holds it.
func (c *Client) scrub(err error) error {
	if c.Key == "" {
		return err
	}
	msg := err.Error()
	if strings.Contains(msg, c.Key) || strings.Contains(msg, url.PathEscape(c.Key)) || strings.Contains(msg, url.QueryEscape(c.Key)) {
		return errors.New("invalid request")
	}
	return err
}

// criteriaHas reports whether criteria is a map with a string key equal to
// choice. A criteria value that is not a string-keyed map is not checked.
func criteriaHas(criteria any, choice string) bool {
	v := reflect.ValueOf(criteria)
	if v.Kind() != reflect.Map || v.Type().Key().Kind() != reflect.String {
		return true
	}
	return v.MapIndex(reflect.ValueOf(choice).Convert(v.Type().Key())).IsValid()
}

// detail makes an error body into one short line without the key. Only the
// "detail" of a 400 or 422 is kept (a string, or msg and loc of list items);
// anything else is the status text.
func (c *Client) detail(status int, data []byte) string {
	s := ""
	if status == http.StatusBadRequest || status == http.StatusUnprocessableEntity {
		var d struct {
			Detail json.RawMessage `json:"detail"`
		}
		if json.Unmarshal(data, &d) == nil {
			var str string
			var items []struct {
				Loc []any  `json:"loc"`
				Msg string `json:"msg"`
			}
			switch {
			case json.Unmarshal(d.Detail, &str) == nil:
				s = str
			case json.Unmarshal(d.Detail, &items) == nil:
				var parts []string
				for _, it := range items {
					p := it.Msg
					if len(it.Loc) > 0 {
						loc := make([]string, len(it.Loc))
						for i, l := range it.Loc {
							loc[i] = fmt.Sprint(l)
						}
						p = strings.Join(loc, ".") + ": " + p
					}
					parts = append(parts, p)
				}
				s = strings.Join(parts, "; ")
			}
		}
	}
	if c.Key != "" {
		for _, k := range []string{c.Key, url.PathEscape(c.Key), url.QueryEscape(c.Key)} {
			s = strings.ReplaceAll(s, k, "[key]")
		}
	}
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) == 0 {
		return statusText(status)
	}
	if len(r) > detailRunes {
		r = r[:detailRunes]
	}
	return string(r)
}

func statusText(status int) string {
	if status == 529 {
		return "Overloaded"
	}
	if t := http.StatusText(status); t != "" {
		return t
	}
	return "error"
}
