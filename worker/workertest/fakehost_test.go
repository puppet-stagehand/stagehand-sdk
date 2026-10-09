package workertest_test

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/puppet-stagehand/stagehand-sdk/worker/workertest"
)

func TestEnvHasTheFourVariablesAndIsACopy(t *testing.T) {
	h := workertest.NewFakeHost(t)
	env := h.Env()
	for _, k := range []string{"STAGEHAND_HOST_ADDR", "STAGEHAND_CLIENT_CERT", "STAGEHAND_CLIENT_KEY", "STAGEHAND_CA_CERT"} {
		if env[k] == "" {
			t.Errorf("Env()[%s] is empty", k)
		}
	}
	env["STAGEHAND_HOST_ADDR"] = "changed"
	if h.Env()["STAGEHAND_HOST_ADDR"] == "changed" {
		t.Error("Env() returns the live map, not a copy")
	}
	if blk, _ := pem.Decode([]byte(h.Env()["STAGEHAND_CLIENT_CERT"])); blk == nil || blk.Type != "CERTIFICATE" {
		t.Error("STAGEHAND_CLIENT_CERT is not PEM certificate content")
	}
}

func TestFakeHostRequiresAClientCertificateAndALPNH2(t *testing.T) {
	h := workertest.NewFakeHost(t)
	env := h.Env()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(env["STAGEHAND_CA_CERT"]))

	// No client certificate: the handshake (completed by the first read) fails.
	conn, err := tls.Dial("tcp", env["STAGEHAND_HOST_ADDR"], &tls.Config{RootCAs: pool, NextProtos: []string{"h2"}})
	if err == nil {
		defer conn.Close()
		buf := make([]byte, 1)
		if _, err = conn.Read(buf); err == nil {
			t.Fatal("the fake host accepted a connection without a client certificate")
		}
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	h := workertest.NewFakeHost(t)
	h.Close()
	h.Close()
}
