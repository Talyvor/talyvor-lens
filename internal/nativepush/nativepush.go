// Package nativepush sends notifications to the phone app: APNs for iPhones (HTTP/2, a token signed with the team's
// .p8 key) and FCM's HTTP v1 API for Android phones (an OAuth access token from a service account).
//
// B40.15 uses it, beside web push, to tell each of a workspace's phones that an approval is waiting: the payee and the
// amount, never account details.
package nativepush

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// ErrGone: the push service says the token no longer reaches an app; forget it.
var ErrGone = errors.New("nativepush: the device token is gone")

// Message is what a phone shows; Data travels with it for the app to open the right screen.
type Message struct {
	Title string
	Body  string
	Data  map[string]string
}

var b64 = base64.RawURLEncoding

// ValidToken reports whether token can be a push token of platform ("ios": APNs, hex; "android": an FCM registration
// token). It goes into a URL path or a JSON body, so nothing else is accepted.
func ValidToken(platform, token string) bool {
	switch platform {
	case "ios":
		_, err := hex.DecodeString(token)
		return err == nil && len(token) >= 64 && len(token) <= 200
	case "android":
		if token == "" || len(token) > 4096 {
			return false
		}
		for _, c := range token {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && !strings.ContainsRune("-_:.", c) {
				return false
			}
		}
		return true
	}
	return false
}

// Secret reads a setting that is either the secret itself (a PEM block, or JSON) or the path of a file holding it.
func Secret(v string) ([]byte, error) {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "-----BEGIN") || strings.HasPrefix(v, "{") {
		return []byte(strings.ReplaceAll(v, `\n`, "\n")), nil
	}
	return os.ReadFile(v)
}

// Senders sends to whichever services are configured; a nil one sends nothing and fails nothing.
type Senders struct {
	APNs *APNs
	FCM  *FCM
}

// Send sends m to token on platform's service.
func (s Senders) Send(ctx context.Context, platform, token string, m Message) error {
	switch {
	case platform == "ios" && s.APNs != nil:
		return s.APNs.Send(ctx, token, m)
	case platform == "android" && s.FCM != nil:
		return s.FCM.Send(ctx, token, m)
	}
	return nil
}

func post(ctx context.Context, client *http.Client, endpoint string, header http.Header, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header = header
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	return resp.StatusCode, out, nil
}

// APNs sends through Apple's push service with a provider token (ES256, kid = the key's id, iss = the team's id).
type APNs struct {
	key           *ecdsa.PrivateKey
	keyID, teamID string
	topic         string // the app's bundle id
	// Host is https://api.push.apple.com, or api.sandbox.push.apple.com for development builds; tests point it at a
	// fake.
	Host   string
	Client *http.Client

	mu      sync.Mutex
	token   string
	tokenAt time.Time
}

// APNs hosts.
const (
	APNsProduction = "https://api.push.apple.com"
	APNsSandbox    = "https://api.sandbox.push.apple.com"
)

// NewAPNs reads the .p8 key (PKCS#8 PEM) Apple issued with keyID for teamID; topic is the app's bundle id.
func NewAPNs(keyPEM []byte, keyID, teamID, topic, host string) (*APNs, error) {
	if keyID == "" || teamID == "" || topic == "" {
		return nil, errors.New("nativepush: APNs needs the key id, the team id and the topic (the app's bundle id)")
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, errors.New("nativepush: the APNs key is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("nativepush: the APNs key: %w", err)
	}
	ec, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("nativepush: the APNs key is not a P-256 key")
	}
	return &APNs{key: ec, keyID: keyID, teamID: teamID, topic: topic, Host: host, Client: &http.Client{Timeout: 15 * time.Second}}, nil
}

// providerToken is the bearer token, kept for 50 minutes: Apple refuses one older than an hour and too many new ones.
func (a *APNs) providerToken() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.token != "" && time.Since(a.tokenAt) < 50*time.Minute {
		return a.token, nil
	}
	header, _ := json.Marshal(map[string]string{"alg": "ES256", "kid": a.keyID})
	claims, _ := json.Marshal(map[string]any{"iss": a.teamID, "iat": time.Now().Unix()})
	input := b64.EncodeToString(header) + "." + b64.EncodeToString(claims)
	h := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, a.key, h[:])
	if err != nil {
		return "", err
	}
	raw := make([]byte, 64)
	r.FillBytes(raw[:32])
	s.FillBytes(raw[32:])
	a.token, a.tokenAt = input+"."+b64.EncodeToString(raw), time.Now()
	return a.token, nil
}

// Send posts m to the device token. ErrGone means the token should be forgotten.
func (a *APNs) Send(ctx context.Context, token string, m Message) error {
	if !ValidToken("ios", token) {
		return errors.New("nativepush: not an APNs device token")
	}
	bearer, err := a.providerToken()
	if err != nil {
		return err
	}
	payload := map[string]any{"aps": map[string]any{"alert": map[string]string{"title": m.Title, "body": m.Body}, "sound": "default"}}
	for k, v := range m.Data {
		payload[k] = v
	}
	body, _ := json.Marshal(payload)
	h := http.Header{}
	h.Set("Authorization", "bearer "+bearer)
	h.Set("apns-topic", a.topic)
	h.Set("apns-push-type", "alert")
	h.Set("apns-priority", "10")
	h.Set("apns-expiration", fmt.Sprint(time.Now().Add(24*time.Hour).Unix()))
	h.Set("Content-Type", "application/json")
	status, out, err := post(ctx, a.Client, a.Host+"/3/device/"+token, h, body)
	if err != nil {
		return fmt.Errorf("nativepush: APNs: %w", err)
	}
	var reply struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(out, &reply)
	switch {
	case status == http.StatusGone || status == http.StatusBadRequest && (reply.Reason == "BadDeviceToken" || reply.Reason == "DeviceTokenNotForTopic"):
		return ErrGone
	case status != http.StatusOK:
		return fmt.Errorf("nativepush: APNs answered %d %s", status, reply.Reason)
	}
	return nil
}

// FCM sends through Firebase Cloud Messaging's HTTP v1 API as a service account.
type FCM struct {
	projectID, email, tokenURI string
	key                        *rsa.PrivateKey
	// Host is https://fcm.googleapis.com; tests point it at a fake.
	Host   string
	Client *http.Client

	mu      sync.Mutex
	access  string
	expires time.Time
}

// NewFCM reads a service account's JSON key (project_id, client_email, private_key, token_uri).
func NewFCM(credentials []byte) (*FCM, error) {
	var c struct {
		ProjectID   string `json:"project_id"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal(credentials, &c); err != nil {
		return nil, fmt.Errorf("nativepush: the FCM credentials are not a service account's JSON key: %w", err)
	}
	if c.ProjectID == "" || c.ClientEmail == "" {
		return nil, errors.New("nativepush: the FCM credentials have no project_id or client_email")
	}
	if c.TokenURI == "" {
		c.TokenURI = "https://oauth2.googleapis.com/token"
	}
	block, _ := pem.Decode([]byte(c.PrivateKey))
	if block == nil {
		return nil, errors.New("nativepush: the FCM credentials' private_key is not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("nativepush: the FCM private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("nativepush: the FCM private key is not an RSA key")
	}
	return &FCM{projectID: c.ProjectID, email: c.ClientEmail, tokenURI: c.TokenURI, key: rk, Host: "https://fcm.googleapis.com",
		Client: &http.Client{Timeout: 15 * time.Second}}, nil
}

// accessToken trades a signed assertion for an access token (RFC 7523), kept until a minute before it expires.
func (f *FCM) accessToken(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.access != "" && time.Now().Before(f.expires) {
		return f.access, nil
	}
	now := time.Now()
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"iss": f.email, "scope": "https://www.googleapis.com/auth/firebase.messaging",
		"aud": f.tokenURI, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	input := b64.EncodeToString(header) + "." + b64.EncodeToString(claims)
	h := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {input + "." + b64.EncodeToString(sig)}}
	hd := http.Header{}
	hd.Set("Content-Type", "application/x-www-form-urlencoded")
	status, out, err := post(ctx, f.Client, f.tokenURI, hd, []byte(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("nativepush: FCM access token: %w", err)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if status != http.StatusOK || json.Unmarshal(out, &tok) != nil || tok.AccessToken == "" {
		return "", fmt.Errorf("nativepush: FCM access token: the token endpoint answered %d", status)
	}
	f.access, f.expires = tok.AccessToken, now.Add(time.Duration(tok.ExpiresIn)*time.Second-time.Minute)
	return f.access, nil
}

// Send posts m to the registration token. ErrGone means the token should be forgotten.
func (f *FCM) Send(ctx context.Context, token string, m Message) error {
	if !ValidToken("android", token) {
		return errors.New("nativepush: not an FCM registration token")
	}
	access, err := f.accessToken(ctx)
	if err != nil {
		return err
	}
	data := map[string]string{}
	for k, v := range m.Data {
		data[k] = v
	}
	body, _ := json.Marshal(map[string]any{"message": map[string]any{"token": token,
		"notification": map[string]string{"title": m.Title, "body": m.Body}, "data": data, "android": map[string]string{"priority": "HIGH"}}})
	h := http.Header{}
	h.Set("Authorization", "Bearer "+access)
	h.Set("Content-Type", "application/json")
	status, out, err := post(ctx, f.Client, f.Host+"/v1/projects/"+url.PathEscape(f.projectID)+"/messages:send", h, body)
	if err != nil {
		return fmt.Errorf("nativepush: FCM: %w", err)
	}
	switch {
	case status == http.StatusNotFound || bytes.Contains(out, []byte(`"UNREGISTERED"`)):
		return ErrGone
	case status != http.StatusOK:
		return fmt.Errorf("nativepush: FCM answered %d", status)
	}
	return nil
}
