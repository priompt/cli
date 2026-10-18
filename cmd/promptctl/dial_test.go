package main

import "testing"

func TestTransportCreds(t *testing.T) {
	c, err := transportCreds(false, "")
	if err != nil || c.Info().SecurityProtocol != "insecure" {
		t.Errorf("no -tls: got %v, %v; want insecure", c, err)
	}
	// -tls without -ca-cert verifies against the system roots instead of
	// failing to open the file "".
	c, err = transportCreds(true, "")
	if err != nil {
		t.Fatalf("-tls without -ca-cert: %v", err)
	}
	if p := c.Info().SecurityProtocol; p != "tls" {
		t.Errorf("-tls without -ca-cert: protocol %q, want tls", p)
	}
	if _, err := transportCreds(true, "does-not-exist.pem"); err == nil {
		t.Error("-tls -ca-cert with a missing file: want an error")
	}
}
