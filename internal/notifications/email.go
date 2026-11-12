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

const (
	addressLimit       = 512
	recipientLimit     = 50
	subjectPrefixLimit = 200
	credentialLimit    = 512
)

type emailConfiguration struct {
	from          *mail.Address
	to            []*mail.Address
	subjectPrefix string
	roots         *x509.CertPool
}

type mailer struct {
	emailConfiguration
	implicitTLS bool
	address     string
	hostname    string
}

type email struct {
	mailer
	auth smtp.Auth
}

func smtpTarget(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" ||
		strings.Trim(u.Path, "/") != "" {
		return nil, errors.New("email destination must be smtps://host:port or smtp+starttls://host:port")
	}
	switch u.Scheme {
	case "smtps", "smtp+starttls":
	default:
		return nil, errors.New("email destination must be smtps://host:port or smtp+starttls://host:port")
	}
	if u.Port() == "" {
		return nil, errors.New("email destination requires an explicit port")
	}
	return u, nil
}

func emailConfig(fields map[string]json.RawMessage) (emailConfiguration, error) {
	if err := onlyFields(fields, "from", "to", "subject_prefix", "ca_certificate"); err != nil {
		return emailConfiguration{}, err
	}
	sender, err := requiredTextField(fields, "from", addressLimit)
	if err != nil {
		return emailConfiguration{}, err
	}
	from, err := mail.ParseAddress(sender)
	if err != nil {
		return emailConfiguration{}, errors.New("from is not a valid address")
	}
	to, err := emailRecipients(fields["to"])
	if err != nil {
		return emailConfiguration{}, err
	}
	subjectPrefix, err := textField(fields, "subject_prefix", subjectPrefixLimit)
	if err != nil {
		return emailConfiguration{}, err
	}
	roots, err := emailRoots(fields)
	if err != nil {
		return emailConfiguration{}, err
	}
	return emailConfiguration{from: from, to: to, subjectPrefix: subjectPrefix, roots: roots}, nil
}

func emailRecipients(raw json.RawMessage) ([]*mail.Address, error) {
	var recipients []string
	if err := json.Unmarshal(raw, &recipients); err != nil || len(recipients) < 1 || len(recipients) > recipientLimit {
		return nil, errors.New("to requires from 1 to 50 addresses")
	}
	to := make([]*mail.Address, 0, len(recipients))
	for _, recipient := range recipients {
		if !headerSafe(recipient, addressLimit) {
			return nil, errors.New("to is invalid")
		}
		address, err := mail.ParseAddress(recipient)
		if err != nil {
			return nil, errors.New("to is not a valid address")
		}
		to = append(to, address)
	}
	return to, nil
}

func emailRoots(fields map[string]json.RawMessage) (*x509.CertPool, error) {
	var certificate string
	if raw, present := fields["ca_certificate"]; present && json.Unmarshal(raw, &certificate) != nil {
		return nil, errors.New("ca_certificate must be a PEM string")
	}
	if certificate == "" {
		return nil, nil
	}
	roots := systemRoots()
	if !roots.AppendCertsFromPEM([]byte(certificate)) {
		return nil, errors.New("ca_certificate is not valid PEM")
	}
	return roots, nil
}

func systemRoots() *x509.CertPool {
	roots, err := x509.SystemCertPool()
	if err != nil {
		return x509.NewCertPool()
	}
	return roots
}

func parseEmail(policy *egress.Policy, d destination) (endpoint[credentialFields], error) {
	target, err := smtpTarget(d.URL)
	if err != nil {
		return nil, err
	}
	if err := policy.ValidateHost(target.Host); err != nil {
		return nil, err
	}
	config, err := emailConfig(d.Configuration)
	if err != nil {
		return nil, err
	}
	return mailer{
		emailConfiguration: config,
		implicitTLS:        target.Scheme == "smtps",
		address:            target.Host,
		hostname:           target.Hostname(),
	}, nil
}

func (m mailer) bind(_ *egress.Policy, fields credentialFields) (channel, error) {
	if err := onlyFields(fields, "username", "password"); err != nil {
		return nil, err
	}
	if len(fields) == 0 {
		return email{mailer: m}, nil
	}
	username, err := requiredTextField(fields, "username", credentialLimit)
	if err != nil {
		return nil, errors.New("email secret requires both username and password or neither")
	}
	password, err := requiredTextField(fields, "password", credentialLimit)
	if err != nil {
		return nil, errors.New("email secret requires both username and password or neither")
	}
	return email{mailer: m, auth: smtp.PlainAuth("", username, password, m.hostname)}, nil
}

func smtpError(err error) error {
	var reply *textproto.Error
	if errors.As(err, &reply) {
		if reply.Code >= 500 {
			return &sendError{smtp5xxCode, err}
		}
		if reply.Code >= 400 {
			return &sendError{smtp4xxCode, err}
		}
	}
	return transportError(err)
}

func (e email) send(ctx context.Context, s *Sender, m message) error {
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
	conn, err := e.secure(sendCtx, rawConn, tlsConfig)
	if err != nil {
		return transportError(err)
	}
	client, err := smtp.NewClient(conn, e.hostname)
	if err != nil {
		return smtpError(err)
	}
	defer client.Close()
	if err := client.Hello("localhost"); err != nil {
		return smtpError(err)
	}
	if !e.implicitTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return &sendError{invalidDestinationCode, errors.New("server does not offer STARTTLS")}
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return smtpError(err)
		}
	}
	if e.auth != nil {
		if err := client.Auth(e.auth); err != nil {
			return &sendError{credentialCode, err}
		}
	}
	if err := client.Mail(e.from.Address); err != nil {
		return smtpError(err)
	}
	for _, recipient := range e.to {
		if err := client.Rcpt(recipient.Address); err != nil {
			return smtpError(err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return smtpError(err)
	}
	if _, err := w.Write(e.message(m)); err != nil {
		_ = w.Close()
		return smtpError(err)
	}
	if err := w.Close(); err != nil {
		return smtpError(err)
	}
	_ = client.Quit()
	return nil
}

func (e email) secure(ctx context.Context, conn net.Conn, config *tls.Config) (net.Conn, error) {
	if !e.implicitTLS {
		return conn, nil
	}
	tlsConn := tls.Client(conn, config)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	return tlsConn, nil
}

func (e email) message(m message) []byte {
	subject := strings.TrimSpace(e.subjectPrefix + " " + m.title)
	to := make([]string, 0, len(e.to))
	for _, recipient := range e.to {
		to = append(to, recipient.String())
	}
	domain := e.from.Address[strings.LastIndex(e.from.Address, "@")+1:]
	var message bytes.Buffer
	_, _ = io.WriteString(&message,
		"From: "+e.from.String()+"\r\n"+
			"To: "+strings.Join(to, ", ")+"\r\n"+
			"Date: "+m.date.UTC().Format(time.RFC1123Z)+"\r\n"+
			"Message-ID: <"+uuid.NewString()+"@"+domain+">\r\n"+
			"Subject: "+mime.QEncoding.Encode("utf-8", truncate(subject, 200))+"\r\n"+
			"MIME-Version: 1.0\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n"+
			"Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	qp := quotedprintable.NewWriter(&message)
	_, _ = io.WriteString(qp, truncate(m.details(), 60000)+"\r\n")
	_ = qp.Close()
	return message.Bytes()
}
