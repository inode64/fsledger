package notify

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"

	"github.com/inode64/fsledger/internal/fault"
)

// emailDeliveryError identifies actionable transport failures without exposing
// server responses, credentials, addresses or certificate contents in durable errors.
func emailDeliveryError(err error) error {
	if err == nil {
		return nil
	}

	var (
		hostname    x509.HostnameError
		certificate *tls.CertificateVerificationError
		network     net.Error
	)

	switch {
	case errors.As(err, &hostname):
		return fault.New("email delivery failed: TLS certificate hostname mismatch; check tls_server_name")
	case errors.As(err, &certificate):
		return fault.New("email delivery failed: TLS certificate verification failed")
	case errors.Is(err, context.Canceled):
		return fault.New("email delivery failed: operation cancelled")
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &network) && network.Timeout():
		return fault.New("email delivery failed: transport timeout")
	}

	// go-mail v0.8.1 has no typed error for missing STARTTLS. Match only its
	// complete fixed message; all other errors keep the sanitized fallback.
	const missingSTARTTLS = `STARTTLS mode set to: "TLSMandatory", but target host does not support STARTTLS`
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if cause.Error() == missingSTARTTLS {
			return fault.New("email delivery failed: SMTP server does not support required STARTTLS")
		}
	}

	return fault.New("email delivery failed")
}
