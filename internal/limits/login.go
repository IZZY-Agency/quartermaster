package limits

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Login is who Codex is logged in as, in hashes: the account (a ChatGPT
// workspace, of which several people can be members) and, when it can be told,
// the membership in it, as usage is counted per member. The zero Login is one
// that cannot be told. It is taken once, before the run or the read it is for,
// and passed on; nothing asks afterwards who it was.
type Login struct {
	account string // hash of the account id
	member  string // hash of the account and user ids; "" when not known
	asked   bool   // Codex named it, as there was no auth.json
}

func newLogin(account, user string) Login {
	l := Login{account: hashAccount(account)}
	if account != "" && user != "" {
		l.member = hashAccount(account + "\x00" + user)
	}
	return l
}

// Known is false for a login that cannot be told.
func (l Login) Known() bool { return l.account != "" }

// Differs reports whether l and o are certainly two logins: both are known,
// and they are of two accounts or of two members of one. Where either side
// names no member the account decides.
func (l Login) Differs(o Login) bool {
	if l.account == "" || o.account == "" {
		return false
	}
	return l.account != o.account || (l.member != "" && o.member != "" && l.member != o.member)
}

// CodexLogin is who the codex at path is logged in as now: what auth.json
// names, else, when there is no auth.json (a login kept in a keyring) and read
// allows to ask, the account Codex itself names, in an answer to a question
// asked after the call.
func CodexLogin(path string, read CodexRead) Login {
	home, err := codexHome()
	if err != nil {
		return Login{}
	}
	login := readLogin(home)
	if login.Known() || read != CodexAsk {
		return login
	}
	if hasAuth(home) {
		return login
	}
	f, fresh, _ := currentUsage(path, home, login, true, time.Now())
	if login = readLogin(home); login.Known() || hasAuth(home) || !fresh {
		return login // an auth.json was written meanwhile
	}
	return f.owner()
}

// Current reports whether Codex still has the login l, as far as that can be
// told without asking it: auth.json names the same one, or, for a login Codex
// named, there still is none.
func (l Login) Current() bool {
	home, err := codexHome()
	if err != nil || !l.Known() {
		return false
	}
	if l.asked {
		return !hasAuth(home)
	}
	return readLogin(home) == l
}

// hasAuth is false when Codex keeps its login elsewhere, or has none.
func hasAuth(home string) bool {
	_, err := os.Lstat(filepath.Join(home, "auth.json"))
	return !errors.Is(err, fs.ErrNotExist)
}

// readLogin is the login auth.json names: tokens.account_id and the user id in
// the payload of tokens.id_token. It is unknown when the file is missing,
// unreadable or names no account (an API-key login). Nothing else of the file
// is decoded, and only hashes leave this function. A file that is not JSON is
// read again a few times: a tool that copies another over it may be half done.
func readLogin(home string) Login {
	for try := 0; ; try++ {
		data, err := os.ReadFile(filepath.Join(home, "auth.json"))
		if err != nil {
			return Login{}
		}
		if json.Valid(data) || try == 3 {
			return parseLogin(data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func parseLogin(data []byte) Login {
	var a struct {
		Tokens struct {
			AccountID string          `json:"account_id"`
			IDToken   json.RawMessage `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &a) != nil {
		return Login{}
	}
	return newLogin(a.Tokens.AccountID, tokenUser(a.Tokens.IDToken))
}

// tokenUser is the ChatGPT user id an id token (a JWT, as a JSON string) is
// for, or "". The token is not verified: it only tells logins apart.
func tokenUser(raw json.RawMessage) string {
	var jwt string
	if json.Unmarshal(raw, &jwt) != nil {
		return ""
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			ChatGPTUserID string `json:"chatgpt_user_id"`
			UserID        string `json:"user_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if claims.Auth.ChatGPTUserID != "" {
		return claims.Auth.ChatGPTUserID
	}
	return claims.Auth.UserID
}

// hashAccount is an id as it is stored; "" stays "".
func hashAccount(id string) string {
	if id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}
