package notifications

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/egress"
)

const (
	channelSecretMax  = 16384
	channelConfigMax  = 4096
	webhookSecretMax  = 1024
	channelTimeout    = 15 * time.Second
	channelDrainBytes = 64 << 10
	pagerDutyAckMax   = 64 << 10
	pagerDutyDedupMax = 255
	discordContentMax = 2000
	discordTitleMax   = 256
	discordDescMax    = 4096
	summaryMax        = 1024
)

type Destination struct {
	Type          string
	URL           string
	Configuration json.RawMessage
}

type ChannelError struct {
	Code string
	Err  error
}

func (e *ChannelError) Error() string { return e.Code }
func (e *ChannelError) Unwrap() error { return e.Err }
func channelErr(code string, err error) error {
	return &ChannelError{Code: code, Err: err}
}

func sendErr(err error) error {
	if err == nil {
		return nil
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return channelErr("timeout", err)
	}
	return channelErr("network", err)
}

func SendErrorCode(err error) string {
	var ce *ChannelError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return "network"
}

func channelType(d Destination) string {
	if d.Type == "" {
		return "webhook"
	}
	return d.Type
}

func strictObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, errors.New("not an object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var decoded map[string]json.RawMessage
	if err := dec.Decode(&decoded); err != nil || decoded == nil {
		return nil, errors.New("not an object")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return decoded, nil
}

func textField(fields map[string]json.RawMessage, name string, max int) (string, error) {
	raw, present := fields[name]
	if !present {
		return "", nil
	}
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return "", errors.New("not a string")
	}
	if len(*value) > max || strings.ContainsAny(*value, "\r\n\x00") {
		return "", errors.New("invalid length or control characters")
	}
	return *value, nil
}

func RequiresSecret(typ string) bool {
	switch typ {
	case "slack", "msteams", "discord", "pagerduty":
		return true
	}
	return false
}

func ValidateDestinationShape(policy *egress.Policy, d Destination) error {
	if policy == nil {
		return errors.New("egress policy unavailable")
	}
	if len(d.Configuration) > channelConfigMax {
		return errors.New("configuration too large")
	}
	switch channelType(d) {
	case "webhook":
		if len(d.Configuration) != 0 {
			c, err := strictObject(d.Configuration)
			if err != nil || len(c) != 0 {
				return errors.New("webhook destinations carry no configuration")
			}
		}
		_, err := policy.ValidateEndpoint(d.URL)
		return err
	case "slack", "msteams", "discord":
		origin, err := channelOrigin(d.URL)
		if err != nil {
			return err
		}
		if len(d.Configuration) != 0 {
			c, err := strictObject(d.Configuration)
			if err != nil || len(c) != 0 {
				return errors.New("chat destinations carry no configuration")
			}
		}
		_, err = policy.ValidateEndpoint(origin.String())
		return err
	case "pagerduty":
		if len(d.Configuration) != 0 {
			c, err := strictObject(d.Configuration)
			if err != nil || len(c) != 0 {
				return errors.New("pagerduty destinations carry no configuration")
			}
		}
		_, err := policy.ValidateEndpoint(d.URL)
		return err
	case "email":
		_, host, err := smtpTarget(d.URL)
		if err != nil {
			return err
		}
		if _, err = policy.ValidateEndpoint("https://" + host); err != nil {
			return err
		}
		config, err := emailConfig(d.Configuration)
		if err != nil {
			return err
		}
		if _, err = mail.ParseAddress(config.From); err != nil {
			return errors.New("from is not a valid address")
		}
		for _, to := range config.To {
			if _, err = mail.ParseAddress(to); err != nil {
				return errors.New("to is not a valid address")
			}
		}
		if config.CACertificate != "" {
			if _, err = emailRoots(config.CACertificate); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New("unsupported destination type")
}

func ValidateDestination(policy *egress.Policy, d Destination, secret []byte) error {
	if len(secret) > channelSecretMax {
		return errors.New("secret too large")
	}
	if err := ValidateDestinationShape(policy, d); err != nil {
		return err
	}
	switch channelType(d) {
	case "webhook":
		if len(secret) > webhookSecretMax {
			return errors.New("secret too large")
		}
		return nil
	case "slack", "msteams", "discord":
		origin, err := channelOrigin(d.URL)
		if err != nil {
			return err
		}
		hook, err := webhookSecretURL(secret, origin)
		if err != nil {
			return err
		}
		queryFree := *hook
		queryFree.RawQuery, queryFree.ForceQuery = "", false
		queryFree.Fragment, queryFree.RawFragment = "", ""
		_, err = policy.ValidateEndpoint(queryFree.String())
		return err
	case "pagerduty":
		if len(secret) == 0 {
			return errors.New("pagerduty requires a routing key secret")
		}
		fields, err := strictObject(secret)
		if err != nil || len(fields) != 1 {
			return errors.New("secret must be a routing_key object")
		}
		key, err := textField(fields, "routing_key", 512)
		if err != nil || key == "" {
			return errors.New("secret must be a routing_key object")
		}
		return nil
	case "email":
		_, _, err := smtpSecret(secret)
		return err
	}
	return errors.New("unsupported destination type")
}

func channelOrigin(raw string) (*url.URL, error) {
	if strings.TrimSpace(raw) != raw {
		return nil, errors.New("destination origin is not a URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil ||
		(u.Scheme != "https" && u.Scheme != "http") ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" {
		return nil, errors.New("destination must be an https://host[:port] origin")
	}
	if path := strings.Trim(u.Path, "/"); path != "" {
		return nil, errors.New("destination must be an https://host[:port] origin")
	}
	u.Path = ""
	u.RawPath = ""
	return u, nil
}

func webhookSecretURL(secret []byte, origin *url.URL) (*url.URL, error) {
	if len(secret) == 0 || len(secret) > channelSecretMax {
		return nil, errors.New("secret must be a webhook_url object")
	}
	fields, err := strictObject(secret)
	if err != nil || len(fields) != 1 {
		return nil, errors.New("secret must be a webhook_url object")
	}
	raw, err := textField(fields, "webhook_url", 2048)
	if err != nil || raw == "" {
		return nil, errors.New("secret must be a webhook_url object")
	}
	hook, err := url.Parse(raw)
	if err != nil || hook.Host == "" || hook.User != nil ||
		(hook.Scheme != "https" && hook.Scheme != "http") ||
		strings.ContainsAny(raw, "\r\n") {
		return nil, errors.New("webhook_url is not an https URL")
	}
	if hook.Scheme+"://"+strings.ToLower(hook.Host) != origin.Scheme+"://"+strings.ToLower(origin.Host) {
		return nil, errors.New("webhook_url must share the destination origin")
	}
	return hook, nil
}

type emailConfiguration struct {
	From          string   `json:"from"`
	To            []string `json:"to"`
	SubjectPrefix string   `json:"subject_prefix,omitempty"`
	CACertificate string   `json:"ca_certificate,omitempty"`
}

func smtpTarget(raw string) (scheme, host string, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" ||
		strings.Trim(u.Path, "/") != "" {
		return "", "", errors.New("email destination must be smtps://host:port or smtp+starttls://host:port")
	}
	switch u.Scheme {
	case "smtps", "smtp+starttls":
	default:
		return "", "", errors.New("email destination must be smtps://host:port or smtp+starttls://host:port")
	}
	if u.Port() == "" {
		return "", "", errors.New("email destination requires an explicit port")
	}
	return u.Scheme, u.Host, nil
}

func emailConfig(raw json.RawMessage) (emailConfiguration, error) {
	var config emailConfiguration
	if len(raw) == 0 {
		return config, errors.New("email destinations require from/to configuration")
	}
	if len(raw) > channelConfigMax {
		return config, errors.New("configuration too large")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&config); err != nil {
		return config, errors.New("configuration fields have invalid types")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return config, errors.New("trailing data")
	}
	if config.From == "" || len(config.From) > 512 || strings.ContainsAny(config.From, "\r\n\x00") {
		return config, errors.New("from is missing or invalid")
	}
	if len(config.To) < 1 || len(config.To) > 50 {
		return config, errors.New("to requires from 1 to 50 addresses")
	}
	for _, to := range config.To {
		if len(to) > 512 || strings.ContainsAny(to, "\r\n\x00") {
			return config, errors.New("to is invalid")
		}
	}
	if len(config.SubjectPrefix) > 200 || strings.ContainsAny(config.SubjectPrefix, "\r\n\x00") {
		return config, errors.New("subject_prefix is invalid")
	}
	if len(config.CACertificate) > channelSecretMax {
		return config, errors.New("ca_certificate too large")
	}
	return config, nil
}

func emailRoots(pemData string) (*x509.CertPool, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM([]byte(pemData)) {
		return nil, errors.New("ca_certificate is not valid PEM")
	}
	return roots, nil
}

func smtpSecret(secret []byte) (string, string, error) {
	if len(secret) == 0 {
		return "", "", nil
	}
	if len(secret) > channelSecretMax {
		return "", "", errors.New("secret too large")
	}
	fields, err := strictObject(secret)
	if err != nil {
		return "", "", errors.New("email secret must be username and password")
	}
	if len(fields) == 0 {
		return "", "", nil
	}
	if len(fields) != 2 {
		return "", "", errors.New("email secret must be username and password")
	}
	user, err := textField(fields, "username", 512)
	if err != nil {
		return "", "", errors.New("email secret must be username and password")
	}
	pass, err := textField(fields, "password", 512)
	if err != nil {
		return "", "", errors.New("email secret must be username and password")
	}
	if user == "" || pass == "" {
		return "", "", errors.New("email secret requires both username and password or neither")
	}
	return user, pass, nil
}

func Send(ctx context.Context, policy *egress.Policy, d Destination, secret []byte, body []byte) error {
	if policy == nil {
		return channelErr("invalid_destination", nil)
	}
	if err := ValidateDestination(policy, d, secret); err != nil {
		return channelErr("invalid_destination", err)
	}
	switch channelType(d) {
	case "webhook":
		return sendWebhook(ctx, policy, d, secret, body)
	case "slack", "msteams", "discord":
		origin, err := channelOrigin(d.URL)
		if err != nil {
			return channelErr("invalid_destination", err)
		}
		hook, err := webhookSecretURL(secret, origin)
		if err != nil {
			return channelErr("invalid_destination", err)
		}
		return postChannel(ctx, policy, hook.String(), channelPayload(d, body))
	case "pagerduty":
		fields, err := strictObject(secret)
		if err != nil {
			return channelErr("invalid_destination", err)
		}
		key, err := textField(fields, "routing_key", 512)
		if err != nil || key == "" {
			return channelErr("invalid_destination", errors.New("routing key unavailable"))
		}
		payload, err := pagerDutyPayload(key, body)
		if err != nil {
			return channelErr("invalid_destination", err)
		}
		return sendPagerDuty(ctx, policy, d.URL, payload)
	case "email":
		return sendEmail(ctx, policy, d, secret, body)
	}
	return channelErr("invalid_destination", errors.New("unsupported destination type"))
}

func sendWebhook(ctx context.Context, policy *egress.Policy, d Destination, secret []byte, body []byte) error {
	target, err := policy.ValidateEndpoint(d.URL)
	if err != nil {
		return channelErr("invalid_destination", err)
	}
	sendCtx, cancel := context.WithTimeout(ctx, channelTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return channelErr("invalid_destination", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if len(secret) != 0 {
		sum := hmac.New(sha256.New, secret)
		sum.Write(body)
		req.Header.Set("X-OLP-Signature", "sha256="+hex.EncodeToString(sum.Sum(nil)))
	}
	return roundTrip(ctx, policy, req)
}

func postChannel(ctx context.Context, policy *egress.Policy, target string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return channelErr("invalid_destination", err)
	}
	sendCtx, cancel := context.WithTimeout(ctx, channelTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return channelErr("invalid_destination", err)
	}
	req.Header.Set("Content-Type", "application/json")
	return roundTrip(ctx, policy, req)
}

func roundTrip(ctx context.Context, policy *egress.Policy, req *http.Request) error {
	client := policy.Client(channelTimeout)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return sendErr(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, channelDrainBytes))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if resp.StatusCode < 500 {
		return channelErr("http_4xx", errors.New("destination refused the request"))
	}
	return channelErr("http_5xx", errors.New("destination failed"))
}

func sendPagerDuty(ctx context.Context, policy *egress.Policy, target string, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return channelErr("invalid_destination", err)
	}
	sendCtx, cancel := context.WithTimeout(ctx, channelTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(sendCtx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return channelErr("invalid_destination", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := policy.Client(channelTimeout)
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return sendErr(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, pagerDutyAckMax))
	if err != nil {
		return sendErr(err)
	}
	if resp.StatusCode != http.StatusAccepted {
		if resp.StatusCode < 500 {
			return channelErr("http_4xx", errors.New("destination refused the request"))
		}
		return channelErr("http_5xx", errors.New("destination failed"))
	}
	var ack struct {
		Status   string `json:"status"`
		DedupKey string `json:"dedup_key"`
	}
	if err := json.Unmarshal(raw, &ack); err != nil || ack.Status != "success" || ack.DedupKey != payload["dedup_key"] {
		return channelErr("http_5xx", errors.New("destination acknowledgement invalid"))
	}
	return nil
}

func mentionSafe(text string) string {
	r := strings.NewReplacer("@", "\uFF20", "<", "\u2039", ">", "\u203A")
	return r.Replace(text)
}

func truncate(text string, max int) string {
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	runes := []rune(text)
	return string(runes[:max-1]) + "…"
}

func bodyFields(body []byte) map[string]json.RawMessage {
	fields := map[string]json.RawMessage{}
	_ = json.Unmarshal(body, &fields)
	return fields
}

func bodyString(fields map[string]json.RawMessage, key string) string {
	raw, present := fields[key]
	if !present {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return value
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String()
	}
	return ""
}

func eventTitle(fields map[string]json.RawMessage) string {
	event, rule := bodyString(fields, "event"), bodyString(fields, "rule_name")
	if rule == "" {
		return mentionSafe(event)
	}
	return mentionSafe(event + " — " + rule)
}

func evidenceText(fields map[string]json.RawMessage, max int) string {
	raw, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return "(evidence unavailable)"
	}
	return mentionSafe(truncate(string(raw), max))
}

func channelPayload(d Destination, body []byte) any {
	fields := bodyFields(body)
	title := eventTitle(fields)
	switch channelType(d) {
	case "slack":
		return map[string]any{
			"text": truncate(title, 150),
			"blocks": []map[string]any{
				{"type": "header", "text": map[string]any{"type": "plain_text", "text": truncate(title, 150)}},
				{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "```" + evidenceText(fields, 2800) + "```"}},
			},
		}
	case "msteams":
		return map[string]any{
			"type": "message",
			"attachments": []map[string]any{
				{"contentType": "application/vnd.microsoft.card.adaptive", "contentUrl": nil,
					"content": map[string]any{
						"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
						"type":    "AdaptiveCard", "version": "1.4",
						"body": []map[string]any{
							{"type": "TextBlock", "text": truncate(title, 256), "weight": "Bolder"},
							{"type": "TextBlock", "text": evidenceText(fields, 3600), "wrap": true},
						},
					}},
			},
		}
	case "discord":
		return map[string]any{
			"content":          truncate(title, discordContentMax),
			"allowed_mentions": map[string]any{"parse": []string{}},
			"embeds": []map[string]any{
				{"title": truncate(title, discordTitleMax), "description": evidenceText(fields, discordDescMax)},
			},
		}
	}
	return nil
}

func pagerDutyPayload(routingKey string, body []byte) (map[string]any, error) {
	fields := bodyFields(body)
	var resolved bool
	_ = json.Unmarshal(fields["resolved"], &resolved)
	dedup := bodyString(fields, "incident_key")
	if dedup == "" {
		dedup = bodyString(fields, "dedup_key")
	}
	if dedup == "" {
		if ruleID, event, window := bodyString(fields, "rule_id"), bodyString(fields, "event"), bodyString(fields, "window_id"); ruleID != "" && event != "" && window != "" {
			dedup = ruleID + ":" + event + ":" + window
		}
	}
	if dedup == "" {
		dedup = bodyString(fields, "delivery_id")
	}
	if len(dedup) > pagerDutyDedupMax {
		return nil, errors.New("deduplication key too long")
	}
	if dedup == "" {
		return nil, errors.New("no deduplication key")
	}
	summary := truncate(eventTitle(fields), summaryMax)
	action := "trigger"
	if resolved {
		action = "resolve"
	}
	payload := map[string]any{
		"routing_key":  routingKey,
		"event_action": action,
		"dedup_key":    dedup,
		"payload": map[string]any{
			"summary":        summary,
			"source":         "OpenLLMProxy",
			"severity":       "warning",
			"custom_details": fields,
		},
	}
	return payload, nil
}

func sendEmail(ctx context.Context, policy *egress.Policy, d Destination, secret []byte, body []byte) error {
	scheme, host, err := smtpTarget(d.URL)
	if err != nil {
		return channelErr("invalid_destination", err)
	}
	config, err := emailConfig(d.Configuration)
	if err != nil {
		return channelErr("invalid_destination", err)
	}
	from, err := mail.ParseAddress(config.From)
	if err != nil {
		return channelErr("invalid_destination", err)
	}
	to := make([]*mail.Address, 0, len(config.To))
	for _, recipient := range config.To {
		address, err := mail.ParseAddress(recipient)
		if err != nil {
			return channelErr("invalid_destination", err)
		}
		to = append(to, address)
	}
	var roots *x509.CertPool
	if config.CACertificate != "" {
		if roots, err = emailRoots(config.CACertificate); err != nil {
			return channelErr("invalid_destination", err)
		}
	} else {
		roots, err = x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
	}
	hostname, _, err := net.SplitHostPort(host)
	if err != nil {
		return channelErr("invalid_destination", err)
	}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: hostname, RootCAs: roots}
	sendCtx, cancel := context.WithTimeout(ctx, channelTimeout)
	defer cancel()
	raw, err := policy.Transport(channelTimeout).DialContext(sendCtx, "tcp", host)
	if err != nil {
		return sendErr(err)
	}
	done := context.AfterFunc(sendCtx, func() { raw.Close() })
	defer done()
	defer raw.Close()
	if deadline, ok := sendCtx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}
	conn := raw
	if scheme == "smtps" {
		conn = tls.Client(raw, tlsConfig)
		if err = conn.(*tls.Conn).HandshakeContext(sendCtx); err != nil {
			return sendErr(err)
		}
	}
	client, err := smtp.NewClient(conn, hostname)
	if err != nil {
		return sendErr(err)
	}
	defer client.Close()
	if scheme == "smtp+starttls" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return channelErr("invalid_destination", errors.New("server does not offer STARTTLS"))
		}
		if err = client.StartTLS(tlsConfig); err != nil {
			return sendErr(err)
		}
	}
	user, pass, err := smtpSecret(secret)
	if err != nil {
		return channelErr("invalid_destination", err)
	}
	if user != "" {
		auth := smtp.PlainAuth("", user, pass, hostname)
		if err = client.Auth(auth); err != nil {
			return channelErr("credential", err)
		}
	}
	if err = client.Mail(from.Address); err != nil {
		return sendErr(err)
	}
	for _, recipient := range to {
		if err = client.Rcpt(recipient.Address); err != nil {
			return sendErr(err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return sendErr(err)
	}
	fields := bodyFields(body)
	subject := config.SubjectPrefix + " " + eventTitle(fields)
	subject = strings.TrimSpace(subject)
	header := "From: " + from.String() + "\r\n" +
		"To: " + strings.Join(config.To, ", ") + "\r\n" +
		"Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n" +
		"Message-ID: <" + uuid.NewString() + "@" + hostname + ">\r\n" +
		"Subject: " + mime.QEncoding.Encode("utf-8", truncate(subject, 200)) + "\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n"
	if _, err = io.WriteString(w, header+truncate(string(body), 60000)+"\r\n"); err != nil {
		w.Close()
		return sendErr(err)
	}
	if err = w.Close(); err != nil {
		return sendErr(err)
	}
	return sendErr(client.Quit())
}
