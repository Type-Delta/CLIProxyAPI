package referencecapture

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"time"
)

// Certificate creates a fresh local CA and an interception certificate scoped to hosts.
func Certificate(name string, hosts []string) (tls.Certificate, []byte, error) {
	now := time.Now()
	newTemplate := func(subject string) *x509.Certificate {
		return &x509.Certificate{
			Subject: pkix.Name{CommonName: subject}, NotBefore: now.Add(-time.Minute),
			NotAfter: now.Add(time.Hour),
		}
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	ca := newTemplate(name)
	ca.IsCA, ca.BasicConstraintsValid = true, true
	ca.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature
	ca.SerialNumber, err = rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, caKey.Public(), caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf := newTemplate(hosts[0])
	leaf.DNSNames = hosts
	leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	leaf.KeyUsage = x509.KeyUsageDigitalSignature
	leaf.SerialNumber, err = rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, leafKey.Public(), caKey)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	return tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), nil
}
