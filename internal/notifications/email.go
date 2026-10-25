package notifications

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/egress"
)

type emailConfiguration struct {
	From          string   `json:"from"`
	To            []string `json:"to"`
	SubjectPrefix string   `json:"subject_prefix,omitempty"`
	CACertificate string   `json:"ca_certificate,omitempty"`
}

type email struct {
	implicitTLS   bool
	address       string
	hostname      string
	subjectPrefix string
	from          *mail.Address
	to            []*mail.Address
	roots         *x509.CertPool
	username      string
	password      string
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
	fields, err := decodeObject(secret)
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

func parseSMTPAddresses(config emailConfiguration) (*mail.Address, []*mail.Address, error) {
	from, err := mail.ParseAddress(config.From)
	if err != nil {
		return nil, nil, errors.New("from is not a valid address")
	}
	to := make([]*mail.Address, 0, len(config.To))
	for _, recipient := range config.To {
		address, err := mail.ParseAddress(recipient)
		if err != nil {
			return nil, nil, errors.New("to is not a valid address")
		}
		to = append(to, address)
	}
	return from, to, nil
}

func parseEmail(policy *egress.Policy, d Destination) (email, error) {
	scheme, host, err := smtpTarget(d.URL)
	if err != nil {
		return email{}, err
	}
	if err := validateSMTPHost(policy, host); err != nil {
		return email{}, err
	}
	config, err := emailConfig(d.Configuration)
	if err != nil {
		return email{}, err
	}
	from, to, err := parseSMTPAddresses(config)
	if err != nil {
		return email{}, err
	}
	var roots *x509.CertPool
	if config.CACertificate != "" {
		roots, err = emailRoots(config.CACertificate)
		if err != nil {
			return email{}, err
		}
	}
	hostname, _, err := net.SplitHostPort(host)
	if err != nil {
		return email{}, errors.New("email destination requires an explicit host and port")
	}
	return email{
		implicitTLS:   scheme == "smtps",
		address:       host,
		hostname:      hostname,
		subjectPrefix: config.SubjectPrefix,
		from:          from,
		to:            to,
		roots:         roots,
	}, nil
}

// validateSMTPHost applies provider egress validation to the SMTP hostname.
func validateSMTPHost(policy *egress.Policy, host string) error {
	_, err := policy.ValidateEndpoint("https://" + host)
	return err
}

// smtpError classifies SMTP reply codes and delegates other failures.
func smtpError(err error) error {
	var reply *textproto.Error
	if errors.As(err, &reply) {
		if reply.Code >= 500 {
			return channelError(smtp5xxCode, err)
		}
		if reply.Code >= 400 {
			return channelError(smtp4xxCode, err)
		}
	}
	return transportError(err)
}

func (e email) bind(_ *egress.Policy, secret []byte) (channel, error) {
	username, password, err := smtpSecret(secret)
	if err != nil {
		return nil, err
	}
	e.username = username
	e.password = password
	return e, nil
}

func (e email) send(ctx context.Context, s *Sender, body []byte) error {
	sendCtx, cancel := context.WithTimeout(ctx, channelTimeout)
	defer cancel()
	rawConn, err := s.policy.DialContext(sendCtx, "tcp", e.address)
	if err != nil {
		return transportError(err)
	}
	done := context.AfterFunc(sendCtx, func() { _ = rawConn.Close() })
	defer done()
	defer rawConn.Close()
	if deadline, ok := sendCtx.Deadline(); ok {
		_ = rawConn.SetDeadline(deadline)
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: e.hostname, RootCAs: e.roots}
	conn := net.Conn(rawConn)
	if e.implicitTLS {
		tlsConn := tls.Client(rawConn, tlsConfig)
		if err = tlsConn.HandshakeContext(sendCtx); err != nil {
			return transportError(err)
		}
		conn = tlsConn
	}
	client, err := smtp.NewClient(conn, e.hostname)
	if err != nil {
		return smtpError(err)
	}
	defer client.Close()
	if err = client.Hello("localhost"); err != nil {
		return smtpError(err)
	}
	if !e.implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return channelError(invalidDestinationCode, errors.New("server does not offer STARTTLS"))
		}
		if err = client.StartTLS(tlsConfig); err != nil {
			return smtpError(err)
		}
	}
	if e.username != "" {
		auth := smtp.PlainAuth("", e.username, e.password, e.hostname)
		if err = client.Auth(auth); err != nil {
			return channelError(credentialCode, err)
		}
	}
	if err = client.Mail(e.from.Address); err != nil {
		return smtpError(err)
	}
	for _, recipient := range e.to {
		if err = client.Rcpt(recipient.Address); err != nil {
			return smtpError(err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return smtpError(err)
	}
	if _, err = w.Write(e.message(body, time.Now())); err != nil {
		_ = w.Close()
		return smtpError(err)
	}
	if err = w.Close(); err != nil {
		return smtpError(err)
	}
	// A failed QUIT must not resend a message the server accepted.
	_ = client.Quit()
	return nil
}

func (e email) message(body []byte, now time.Time) []byte {
	fields := bodyFields(body)
	subject := strings.TrimSpace(e.subjectPrefix + " " + eventTitle(fields))
	to := make([]string, 0, len(e.to))
	for _, recipient := range e.to {
		to = append(to, recipient.String())
	}
	domain := ""
	if at := strings.LastIndex(e.from.Address, "@"); at >= 0 {
		domain = e.from.Address[at+1:]
	}
	var message bytes.Buffer
	_, _ = io.WriteString(&message,
		"From: "+e.from.String()+"\r\n"+
			"To: "+strings.Join(to, ", ")+"\r\n"+
			"Date: "+now.UTC().Format(time.RFC1123Z)+"\r\n"+
			"Message-ID: <"+uuid.NewString()+"@"+domain+">\r\n"+
			"Subject: "+mime.QEncoding.Encode("utf-8", truncate(subject, 200))+"\r\n"+
			"MIME-Version: 1.0\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n"+
			"Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	qp := quotedprintable.NewWriter(&message)
	_, _ = io.WriteString(qp, truncate(evidence(fields), 60000)+"\r\n")
	_ = qp.Close()
	return message.Bytes()
}
