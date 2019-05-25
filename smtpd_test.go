package smtpd

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net"
	"net/textproto"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pkg/errors"
)

// Required for MIME parsing
var mimeHeaders = "Content-Type: text/plain\r\n\r\n"

var cert = makeCertificate()

// Create a client to run commands with. Parse the banner for 220 response.
func newConn(t *testing.T, server *Server) net.Conn {
	clientConn, serverConn := net.Pipe()

	serverConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	serverConn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	clientConn.SetReadDeadline(time.Now().Add(2 * time.Second))
	clientConn.SetWriteDeadline(time.Now().Add(2 * time.Second))

	session := server.newSession(serverConn)
	go session.serve()

	code, banner, err := textproto.NewConn(clientConn).ReadCodeLine(220)

	// banner, err := bufio.NewReader(clientConn).ReadString('\n')
	if err != nil {
		t.Fatalf("Failed to read banner from test server: %v", err)
	}
	if code != 220 {
		t.Fatalf("Read incorrect banner from test server: %v", banner)
	}
	return clientConn
}

// Simple wrapper to send and receive command and response
func writeAndExpect(conn net.Conn, send string, code int) (msg string, err error) {
	err = textproto.NewConn(conn).PrintfLine(send)
	if err != nil {
		return
	}

	// Response is one-or-more lines
	// _, msg, err = textproto.NewConn(conn).ReadCodeLine(code)
	_, msg, err = textproto.NewConn(conn).ReadResponse(code)

	if err != nil {
		err = errors.Wrap(err, fmt.Sprintf("sent: %q: want: %d, got", send, code))
	}

	return
}

// Send a command and verify the 3 digit code from the response.
func cmdCode(t *testing.T, conn net.Conn, cmd string, code int) string {
	msg, err := writeAndExpect(conn, cmd, code)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// Simple tests: connect, send command, then send QUIT.
// RFC 2821 section 4.1.4 specifies that these commands do not require a prior EHLO,
// only that clients should send one, so test without EHLO.
func TestSimpleCommands(t *testing.T) {
	var err error
	tests := []struct {
		cmd  string
		code int
	}{
		{"NOOP", 250},
		{"RSET", 250},
		{"HELP", 502},
		{"VRFY", 502},
		{"EXPN", 502},
		{"TEST", 500}, // Unsupported command
		{"", 500},     // Blank command
	}

	for _, tt := range tests {
		conn := newConn(t, &Server{})
		_, err = writeAndExpect(conn, tt.cmd, tt.code)
		if err != nil {
			t.Fatal(err)
		}
		_, err = writeAndExpect(conn, "QUIT", 221)
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
}

func TestCmdHELO(t *testing.T) {
	conn := newConn(t, &Server{})

	// Send HELO, expect greeting.
	cmdCode(t, conn, "HELO host.example.com", 250)

	// Verify that HELO resets the current transaction state like RSET.
	// RFC 2821 section 4.1.4 says EHLO should cause a reset, so verify that HELO does it too.
	cmdCode(t, conn, "MAIL FROM:<sender@example.com>", 250)
	cmdCode(t, conn, "RCPT TO:<recipient@example.com>", 250)
	cmdCode(t, conn, "HELO host.example.com", 250)
	cmdCode(t, conn, "DATA", 503)

	cmdCode(t, conn, "QUIT", 221)
	conn.Close()
}

func TestCmdEHLO(t *testing.T) {
	conn := newConn(t, &Server{})

	// Send EHLO, expect greeting.
	cmdCode(t, conn, "EHLO host.example.com", 250)

	// Verify that EHLO resets the current transaction state like RSET.
	// See RFC 2821 section 4.1.4 for more detail.
	cmdCode(t, conn, "MAIL FROM:<sender@example.com>", 250)
	cmdCode(t, conn, "RCPT TO:<recipient@example.com>", 250)
	cmdCode(t, conn, "EHLO host.example.com", 250)
	cmdCode(t, conn, "DATA", 503)

	cmdCode(t, conn, "QUIT", 221)
	conn.Close()
}

func TestCmdRSET(t *testing.T) {
	conn := newConn(t, &Server{})
	cmdCode(t, conn, "EHLO host.example.com", 250)

	// Verify that RSET clears the current transaction state.
	cmdCode(t, conn, "MAIL FROM:<sender@example.com>", 250)
	cmdCode(t, conn, "RCPT TO:<recipient@example.com>", 250)
	cmdCode(t, conn, "RSET", 250)
	cmdCode(t, conn, "DATA", 503)

	cmdCode(t, conn, "QUIT", 221)
	conn.Close()
}

func TestCmdMAIL(t *testing.T) {
	conn := newConn(t, &Server{})
	cmdCode(t, conn, "EHLO host.example.com", 250)

	// MAIL with no FROM arg should return 501 syntax error
	cmdCode(t, conn, "MAIL", 501)
	// MAIL with empty FROM arg should return 501 syntax error
	cmdCode(t, conn, "MAIL FROM:", 501)
	// MAIL with DSN-style FROM arg should return 250 Ok
	cmdCode(t, conn, "MAIL FROM:<>", 250)
	// MAIL with valid FROM arg should return 250 Ok
	cmdCode(t, conn, "MAIL FROM:<sender@example.com>", 250)

	// MAIL with valid SIZE parameter should return 250 Ok
	cmdCode(t, conn, "MAIL FROM:<sender@example.com> SIZE=1000", 250)

	// MAIL with bad size parameter should return 501 syntax error
	cmdCode(t, conn, "MAIL FROM:<sender@example.com> SIZE", 501)
	cmdCode(t, conn, "MAIL FROM:<sender@example.com> SIZE=", 501)
	cmdCode(t, conn, "MAIL FROM:<sender@example.com> SIZE= ", 501)
	cmdCode(t, conn, "MAIL FROM:<sender@example.com> SIZE=foo", 501)

	cmdCode(t, conn, "QUIT", 221)
	conn.Close()
}

func TestCmdMAILMaxSize(t *testing.T) {
	maxSize := 10 + time.Now().Minute()
	conn := newConn(t, &Server{MaxSize: maxSize})
	cmdCode(t, conn, "EHLO host.example.com", 250)

	// MAIL with no size parameter should return 250 Ok
	cmdCode(t, conn, "MAIL FROM:<sender@example.com>", 250)

	// MAIL with bad size parameter should return 501 syntax error
	cmdCode(t, conn, "MAIL FROM:<sender@example.com> SIZE", 501)
	cmdCode(t, conn, "MAIL FROM:<sender@example.com> SIZE=", 501)
	cmdCode(t, conn, "MAIL FROM:<sender@example.com> SIZE= ", 501)
	cmdCode(t, conn, "MAIL FROM:<sender@example.com> SIZE=foo", 501)

	// MAIL with size parameter zero should return 250 Ok
	cmdCode(t, conn, "MAIL FROM:<sender@example.com> SIZE=0", 250)

	// MAIL below the maximum size should return 250 Ok
	cmdCode(t, conn, fmt.Sprintf("MAIL FROM:<sender@example.com> SIZE=%d", maxSize-1), 250)

	// MAIL matching the maximum size should return 250 Ok
	cmdCode(t, conn, fmt.Sprintf("MAIL FROM:<sender@example.com> SIZE=%d", maxSize), 250)

	// MAIL above the maximum size should return a maximum size exceeded error.
	cmdCode(t, conn, fmt.Sprintf("MAIL FROM:<sender@example.com> SIZE=%d", maxSize+1), 552)

	// Clients should send either RSET or QUIT after receiving 552 (RFC 1870 section 6.2).
	cmdCode(t, conn, "QUIT", 221)
	conn.Close()
}

func TestCmdRCPT(t *testing.T) {
	conn := newConn(t, &Server{})
	cmdCode(t, conn, "EHLO host.example.com", 250)

	// RCPT without prior MAIL should return 503 bad sequence
	cmdCode(t, conn, "RCPT", 503)

	cmdCode(t, conn, "MAIL FROM:<sender@example.com>", 250)

	// RCPT with no TO arg should return 501 syntax error
	cmdCode(t, conn, "RCPT", 501)

	// RCPT with empty TO arg should return 501 syntax error
	cmdCode(t, conn, "RCPT TO:", 501)

	// RCPT with valid TO arg should return 250 Ok
	cmdCode(t, conn, "RCPT TO:<recipient@example.com>", 250)

	// Up to 100 valid recipients should return 250 Ok
	for i := 2; i < 101; i++ {
		cmdCode(t, conn, fmt.Sprintf("RCPT TO:<recipient%v@example.com>", i), 250)
	}

	// 101st valid recipient with valid TO arg should return 452 too many recipients
	cmdCode(t, conn, "RCPT TO:<recipient101@example.com>", 452)

	// RCPT with valid TO arg and prior DSN-style FROM arg should return 250 Ok
	cmdCode(t, conn, "RSET", 250)
	cmdCode(t, conn, "MAIL FROM:<>", 250)
	cmdCode(t, conn, "RCPT TO:<recipient@example.com>", 250)

	cmdCode(t, conn, "QUIT", 221)
	conn.Close()
}

func TestCmdDATA(t *testing.T) {
	conn := newConn(t, &Server{})
	cmdCode(t, conn, "EHLO host.example.com", 250)

	// DATA without prior MAIL & RCPT should return 503 bad sequence
	cmdCode(t, conn, "DATA", 503)
	cmdCode(t, conn, "RSET", 250)

	// DATA without prior RCPT should return 503 bad sequence
	cmdCode(t, conn, "MAIL FROM:<sender@example.com>", 250)
	cmdCode(t, conn, "DATA", 503)
	cmdCode(t, conn, "RSET", 250)

	// Test a full mail transaction.
	cmdCode(t, conn, "MAIL FROM:<sender@example.com>", 250)
	cmdCode(t, conn, "RCPT TO:<recipient@example.com>", 250)
	cmdCode(t, conn, "DATA", 354)
	cmdCode(t, conn, mimeHeaders+"Test message.\r\n.", 250)

	// Test a full mail transaction with a bad last recipient.
	cmdCode(t, conn, "MAIL FROM:<sender@example.com>", 250)
	cmdCode(t, conn, "RCPT TO:<recipient@example.com>", 250)
	cmdCode(t, conn, "RCPT TO:", 501)
	cmdCode(t, conn, "DATA", 354)
	cmdCode(t, conn, mimeHeaders+"Test message.\r\n.", 250)

	cmdCode(t, conn, "QUIT", 221)
	conn.Close()
}

func TestCmdDATAWithMaxSize(t *testing.T) {

	// "Test message.\r\n." is 15 bytes after trailing period is removed.
	conn := newConn(t, &Server{MaxSize: len(mimeHeaders) + 15})
	cmdCode(t, conn, "EHLO host.example.com", 250)

	// Messages below the maximum size should return 250 Ok
	cmdCode(t, conn, "MAIL FROM:<sender@example.com>", 250)
	cmdCode(t, conn, "RCPT TO:<recipient@example.com>", 250)
	cmdCode(t, conn, "DATA", 354)
	cmdCode(t, conn, mimeHeaders+"Test message\r\n.", 250)

	// Messages matching the maximum size should return 250 Ok
	cmdCode(t, conn, "MAIL FROM:<sender@example.com>", 250)
	cmdCode(t, conn, "RCPT TO:<recipient@example.com>", 250)
	cmdCode(t, conn, "DATA", 354)
	cmdCode(t, conn, mimeHeaders+"Test message.\r\n.", 250)

	// Debug = true

	// Messages above the maximum size should return a maximum size exceeded error.
	cmdCode(t, conn, "MAIL FROM:<sender@example.com>", 250)
	cmdCode(t, conn, "RCPT TO:<recipient@example.com>", 250)
	cmdCode(t, conn, "DATA", 354)
	cmdCode(t, conn, mimeHeaders+"Test message that is too long.\r\n.", 552)

	// Clients should send either RSET or QUIT after receiving 552 (RFC 1870 section 6.2).
	cmdCode(t, conn, "RSET", 250)

	// Messages above the maximum size should return a maximum size exceeded error.
	cmdCode(t, conn, "MAIL FROM:<sender@example.com>", 250)
	cmdCode(t, conn, "RCPT TO:<recipient@example.com>", 250)
	cmdCode(t, conn, "DATA", 354)
	cmdCode(t, conn, mimeHeaders+"Test message.\r\nSecond line that is too long.\r\n.", 552)

	// Debug = false

	// Clients should send either RSET or QUIT after receiving 552 (RFC 1870 section 6.2).
	cmdCode(t, conn, "QUIT", 221)
	conn.Close()
}

func TestCmdSTARTTLS(t *testing.T) {
	conn := newConn(t, &Server{})
	cmdCode(t, conn, "EHLO host.example.com", 250)

	// By default, TLS is not configured, so STARTTLS should return 502 not implemented.
	cmdCode(t, conn, "STARTTLS", 502)

	// Parameters are not allowed (RFC 3207 section 4).
	cmdCode(t, conn, "STARTTLS FOO", 501)

	cmdCode(t, conn, "QUIT", 221)
	conn.Close()
}

func TestCmdSTARTTLSFailure(t *testing.T) {
	// Deliberately misconfigure TLS to force a handshake failure.
	server := &Server{TLSConfig: &tls.Config{}}
	conn := newConn(t, server)
	cmdCode(t, conn, "EHLO host.example.com", 250)

	// When TLS is configured, STARTTLS should return 220 Ready to start TLS.
	cmdCode(t, conn, "STARTTLS", 220)

	// A failed TLS handshake should return 403 TLS handshake failed
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
	err := tlsConn.Handshake()
	if err != nil {
		reader := bufio.NewReader(conn)
		resp, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("Failed to read response after failed TLS handshake: %v", err)
		}
		if resp[0:3] != "403" {
			t.Errorf("Failed TLS handshake response code is %s, want 403", resp[0:3])
		}
	} else {
		t.Error("TLS handshake succeeded with empty tls.Config, want failure")
	}

	cmdCode(t, conn, "QUIT", 221)
	tlsConn.Close()
}

// Utility function to make a valid TLS certificate for use by the server.
func makeCertificate() tls.Certificate {
	const certPEM = `
-----BEGIN CERTIFICATE-----
MIID9DCCAtygAwIBAgIJAIX/1sxuqZKrMA0GCSqGSIb3DQEBCwUAMFkxCzAJBgNV
BAYTAkFVMRMwEQYDVQQIEwpTb21lLVN0YXRlMSEwHwYDVQQKExhJbnRlcm5ldCBX
aWRnaXRzIFB0eSBMdGQxEjAQBgNVBAMTCWxvY2FsaG9zdDAeFw0xNzA1MDYxNDIy
MjVaFw0yNzA1MDQxNDIyMjVaMFkxCzAJBgNVBAYTAkFVMRMwEQYDVQQIEwpTb21l
LVN0YXRlMSEwHwYDVQQKExhJbnRlcm5ldCBXaWRnaXRzIFB0eSBMdGQxEjAQBgNV
BAMTCWxvY2FsaG9zdDCCASIwDQYJKoZIhvcNAQEBBQADggEPADCCAQoCggEBALO4
XVY5Kw9eNblqBenC03Wz6qemLFw8zLDNrehvjYuJPn5WVwvzLNP+3S02iqQD+Y1k
vszqDIZLQdjWLiEZdtxfemyIr+RePIMclnceGYFx3Zgg5qeyvOWlJLM41ZU8YZb/
zGj3RtXzuOZ5vePSLGS1nudjrKSBs7shRY8bYjkOqFujsSVnEK7s3Kb2Sf/rO+7N
RZ1df3hhyKtyq4Pb5eC1mtQqcRjRSZdTxva8kO4vRQbvGgjLUakvBVrrnwbww5a4
2wKbQPKIClEbSLyKQ62zR8gW1rPwBdokd8u9+rLbcmr7l0OuAsSn5Xi9x6VxXTNE
bgCa1KVoE4bpoGG+KQsCAwEAAaOBvjCBuzAdBgNVHQ4EFgQUILso/fozIhaoyi05
XNSWzP/ck+4wgYsGA1UdIwSBgzCBgIAUILso/fozIhaoyi05XNSWzP/ck+6hXaRb
MFkxCzAJBgNVBAYTAkFVMRMwEQYDVQQIEwpTb21lLVN0YXRlMSEwHwYDVQQKExhJ
bnRlcm5ldCBXaWRnaXRzIFB0eSBMdGQxEjAQBgNVBAMTCWxvY2FsaG9zdIIJAIX/
1sxuqZKrMAwGA1UdEwQFMAMBAf8wDQYJKoZIhvcNAQELBQADggEBAIbzsvTZb8LA
JqyaTttsMMA1szf4WBX88lVWbIk91k0nlTa0BiU/UocKrU6c9PySwJ6FOFJpgpdH
z/kmJ+S+d4pvgqBzWbKMoMrNlMt6vL+H8Mbf/l/CN91eNM+gJZu2HgBIFGW1y4Wy
gOzjEm9bw15Hgqqs0P4CSy7jcelWA285DJ7IG1qdPGhAKxT4/UuDin8L/u2oeYWH
3DwTDO4kAUnKetcmNQFSX3Ge50uQypl8viYgFJ2axOfZ3imjQZrs7M1Og6Wnj/SD
F414wVQibsZyZp8cqwR/OinvxloPkPVnf163jPRtftuqezEY8Nyj83O5u5sC1Azs
X/Gm54QNk6w=
-----END CERTIFICATE-----`
	const keyPEM = `
-----BEGIN RSA PRIVATE KEY-----
MIIEowIBAAKCAQEAs7hdVjkrD141uWoF6cLTdbPqp6YsXDzMsM2t6G+Ni4k+flZX
C/Ms0/7dLTaKpAP5jWS+zOoMhktB2NYuIRl23F96bIiv5F48gxyWdx4ZgXHdmCDm
p7K85aUkszjVlTxhlv/MaPdG1fO45nm949IsZLWe52OspIGzuyFFjxtiOQ6oW6Ox
JWcQruzcpvZJ/+s77s1FnV1/eGHIq3Krg9vl4LWa1CpxGNFJl1PG9ryQ7i9FBu8a
CMtRqS8FWuufBvDDlrjbAptA8ogKURtIvIpDrbNHyBbWs/AF2iR3y736sttyavuX
Q64CxKfleL3HpXFdM0RuAJrUpWgThumgYb4pCwIDAQABAoIBAHzvYntJPKTvUhu2
F6w8kvHVBABNpbLtVUJniUj3G4fv/bCn5tVY1EX/e9QtgU2psbbYXUdoQRKuiHTr
15+M6zMhcKK4lsYDuL9QhU0DcKmq9WgHHzFfMK/YEN5CWT/ofNMSuhASLn0Xc+dM
pHQWrGPKWk/y25Z0z/P7mjZ0y+BrJOKlxV53A2AWpj4JtjX2YO6s/eiraFX+RNlv
GyWzeQ7Gynm2TD9VXhS+m40VVBmmbbeZYDlziDoWWNe9r26A+C8K65gZtjKdarMd
0LN89jJvI1pUxcIuvZJnumWUenZ7JhfBGpkfAwLB+MogUo9ekAHv1IZv/m3uWq9f
Zml2dZECgYEA2OCI8kkLRa3+IodqQNFrb/uZ16YouQ71B7nBgAxls9nuhyELKO7d
fzf1snPx6cbaCQKTyxrlYvck4gz8P09R7nVYwJuTmP0+QIgeCCc3Y9A2dyExaC6I
uKkFzJEqIVZNLvdjBRWQs5AiD1w58oto+wOvbagAQM483WiJ/qFaHCMCgYEA1CPo
zwI6pCn39RSYffK25HXM1q3i8ypkYdNsG6IVqS2FqHqj8XJSnDvLeIm7W1Rtw+uM
QdZ5O6PH31XgolG6LrFkW9vtfH+QnXQA2AnZQEfn034YZubhcexLqAkS9r0FUUZp
a1WI2jSxBBeB+to6MdNABuQOL3NHjPUidUKnOfkCgYA+HvKbE7ka2F+23DrfHh08
EkFat8lqWJJvCBIY73QiNAZSxnA/5UukqQ7DctqUL9U8R3S19JpH4qq55SZLrBi3
yP0HDokUhVVTfqm7hCAlgvpW3TcdtFaNLjzu/5WlvuaU0V+XkTnFdT+MTsp6YtxL
Kh8RtdF8vpZIhS0htm3tKQKBgQDQXoUp79KRtPdsrtIpw+GI/Xw50Yp9tkHrJLOn
YMlN5vzFw9CMM/KYqtLsjryMtJ0sN40IjhV+UxzbbYq7ZPMvMeaVo6vdAZ+WSH8b
tHDEBtzai5yEVntSXvrhDiimWnuCnVqmptlJG0BT+JMfRoKqtgjJu++DBARfm9hA
vTtsYQKBgE1ttTzd3HJoIhBBSvSMbyDWTED6jecKvsVypb7QeDxZCbIwCkoK9zn1
twPDHLBcUNhHJx6JWTR6BxI5DZoIA1tcKHtdO5smjLWNSKhXTsKWee2aNkZJkNIW
TDHSaTMOxVUEzpx84xClf561BTiTgzQy2MULpg3AK0Cv9l0+Yrvz
-----END RSA PRIVATE KEY-----`

	cert, _ := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	return cert
}

func TestCmdSTARTTLSSuccess(t *testing.T) {
	// Configure a valid TLS certificate so the handshake will succeed.
	server := &Server{TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}}
	conn := newConn(t, server)

	var err error

	// cmdCode(t, conn, "EHLO host.example.com", 250)
	_, err = writeAndExpect(conn, "EHLO host.example.com", 250)
	if err != nil {
		t.Error(err)
	}

	// When TLS is configured, STARTTLS should return 220 Ready to start TLS.
	// cmdCode(t, conn, "STARTTLS", 220)
	_, err = writeAndExpect(conn, "STARTTLS", 220)
	if err != nil {
		t.Error(err)
	}

	// A successful TLS handshake shouldn't return anything, it should wait for EHLO.
	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
	err = tlsConn.Handshake()
	if err != nil {
		t.Errorf("Failed to perform TLS handshake")
	}

	// The subsequent EHLO should be successful.
	_, err = writeAndExpect(tlsConn, "EHLO host.example.com", 250)
	if err != nil {
		t.Fatal(err)
	}

	// When TLS is already in use, STARTTLS should return 503 bad sequence.
	_, err = writeAndExpect(tlsConn, "STARTTLS", 503)
	if err != nil {
		t.Fatal(err)
	}

	_, err = writeAndExpect(tlsConn, "QUIT", 221)
	if err != nil {
		t.Fatal(err)
	}

	tlsConn.Close()
}

// func TestCmdSTARTTLSRequired(t *testing.T) {
// 	tests := []struct {
// 		cmd        string
// 		codeBefore string
// 		codeAfter  string
// 	}{
// 		{"EHLO host.example.com", 250, 250},
// 		{"NOOP", 250, 250},
// 		{"MAIL FROM:<sender@example.com>", 530, 250},
// 		{"RCPT TO:<recipient@example.com>", 530, 250},
// 		{"RSET", 530, 250}, // Reset before DATA to avoid having to actually send a message.
// 		{"DATA", 530, 503},
// 		{"HELP", 502, 502},
// 		{"VRFY", 502, 502},
// 		{"EXPN", 502, 502},
// 		{"TEST", 500, 500}, // Unsupported command
// 		{"", 500, 500},     // Blank command
// 		{"AUTH", 502, 502}, // AUTH is not supported
// 	}
//
// 	// If TLS is not configured, the TLSRequired setting is ignored, so it must be configured for this test.
// 	server := &Server{TLSConfig: &tls.Config{Certificates: []tls.Certificate{cert}}, TLSRequired: true}
// 	conn := newConn(t, server)
//
// 	// If TLS is required, but not in use, reject every command except NOOP, EHLO, STARTTLS, or QUIT as per RFC 3207 section 4.
// 	for _, tt := range tests {
// 		cmdCode(t, conn, tt.cmd, tt.codeBefore)
// 	}
//
// 	// Switch to using TLS.
// 	cmdCode(t, conn, "STARTTLS", 220)
//
// 	// A successful TLS handshake shouldn't return anything, it should wait for EHLO.
// 	tlsConn := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
// 	err := tlsConn.Handshake()
// 	if err != nil {
// 		t.Errorf("Failed to perform TLS handshake")
// 	}
//
// 	// The subsequent EHLO should be successful.
// 	cmdCode(t, tlsConn, "EHLO host.example.com", 250)
//
// 	// If TLS is required, and is in use, every command should work normally.
// 	for _, tt := range tests {
// 		cmdCode(t, tlsConn, tt.cmd, tt.codeAfter)
// 	}
//
// 	cmdCode(t, tlsConn, "QUIT", 221)
// 	tlsConn.Close()
// }

// func TestMakeHeaders(t *testing.T) {
// 	now := time.Now().Format("Mon, _2 Jan 2006 15:04:05 -0700 (MST)")
// 	valid := "Received: from clientName (clientHost [clientIP])\r\n" +
// 		"        by serverName (smtpd) with SMTP\r\n" +
// 		"        for <recipient@example.com>; " +
// 		fmt.Sprintf("%s\r\n", now)
//
// 	srv := &Server{Appname: "smtpd", Hostname: "serverName"}
// 	s := &session{srv: srv, remoteIP: "clientIP", remoteHost: "clientHost", remoteName: "clientName"}
// 	headers := s.makeHeaders([]string{"recipient@example.com"})
// 	if string(headers) != valid {
// 		t.Errorf("makeHeaders() returned\n%v, want\n%v", string(headers), valid)
// 	}
// }

// Test parsing of commands into verbs and arguments.
func TestParseLine(t *testing.T) {
	tests := []struct {
		line string
		verb string
		args string
	}{
		{"EHLO host.example.com", "EHLO", "host.example.com"},
		{"MAIL FROM:<sender@example.com>", "MAIL", "FROM:<sender@example.com>"},
		{"RCPT TO:<recipient@example.com>", "RCPT", "TO:<recipient@example.com>"},
		{"QUIT", "QUIT", ""},
	}
	s := &session{}
	for _, tt := range tests {
		verb, args := s.parseLine(tt.line)
		if verb != tt.verb || args != tt.args {
			t.Errorf("ParseLine(%v) returned %v, %v, want %v, %v", tt.line, verb, args, tt.verb, tt.args)
		}
	}
}

// Test reading of message data, including dot stuffing (see RFC 5321 section 4.5.2).
// func TestReadData(t *testing.T) {
// 	tests := []struct {
// 		lines string
// 		data  string
// 	}{
// 		// Single line message.
// 		{mimeHeaders + "Test message.\r\n.\r\n", mimeHeaders + "Test message.\r\n"},
//
// 		// Single line message with leading period removed.
// 		{mimeHeaders + ".Test message.\r\n.\r\n", mimeHeaders + "Test message.\r\n"},
//
// 		// Multiple line message.
// 		{mimeHeaders + "Line 1.\r\nLine 2.\r\nLine 3.\r\n.\r\n", mimeHeaders + "Line 1.\r\nLine 2.\r\nLine 3.\r\n"},
//
// 		// Multiple line message with leading period removed.
// 		{mimeHeaders + "Line 1.\r\n.Line 2.\r\nLine 3.\r\n.\r\n", mimeHeaders + "Line 1.\r\nLine 2.\r\nLine 3.\r\n"},
//
// 		// Multiple line message with one leading period removed.
// 		{mimeHeaders + "Line 1.\r\n..Line 2.\r\nLine 3.\r\n.\r\n", mimeHeaders + "Line 1.\r\n.Line 2.\r\nLine 3.\r\n"},
// 	}
// 	var buf bytes.Buffer
// 	s := &session{}
// 	s.srv = &Server{}
// 	s.br = bufio.NewReader(&buf)
//
// 	// Ensure readData() returns an EOF error on an empty buffer.
// 	_, err := s.readData()
// 	if err != io.EOF {
// 		t.Errorf("readData() on empty buffer returned err: %v, want EOF", err)
// 	}
//
// 	for _, tt := range tests {
// 		buf.Write([]byte(tt.lines))
// 		data, err := s.readData()
// 		if err != nil {
// 			t.Errorf("readData(%v) returned err: %v", tt.lines, err)
// 		} else if string(data) != tt.data {
// 			t.Errorf("readData(%v) returned %v, want %v", tt.lines, string(data), tt.data)
// 		}
// 	}
// }
//
// // Test reading of message data with maximum size set (see RFC 1870 section 6.3).
// func TestReadDataWithMaxSize(t *testing.T) {
// 	tests := []struct {
// 		lines   string
// 		maxSize int
// 		err     error
// 	}{
// 		// Maximum size of zero (the default) should not return an error.
// 		{mimeHeaders + "Test message.\r\n.\r\n", 0, nil},
//
// 		// Messages below the maximum size should not return an error.
// 		{mimeHeaders + "Test message.\r\n.\r\n", len(mimeHeaders) + 16, nil},
//
// 		// Messages matching the maximum size should not return an error.
// 		{mimeHeaders + "Test message.\r\n.\r\n", len(mimeHeaders) + 15, nil},
//
// 		// Messages above the maximum size should return a maximum size exceeded error.
// 		{mimeHeaders + "From: a@example.com\r\n\r\nTest message.\r\n.\r\n", len(mimeHeaders) + 14, maxSizeExceeded(len(mimeHeaders) + 14)},
// 	}
// 	var buf bytes.Buffer
// 	s := &session{}
// 	s.br = bufio.NewReader(&buf)
//
// 	for _, tt := range tests {
// 		s.srv = &Server{MaxSize: tt.maxSize}
// 		buf.Write([]byte(tt.lines))
// 		_, err := s.readData()
// 		if err != tt.err {
// 			t.Errorf("readData(%v) returned err: %v\n\tExpecting: %v", tt.lines, err, tt.err)
// 		}
// 	}
// }

// Utility function for parsing extensions listed as service extensions in response to an EHLO command.
func parseExtensions(t *testing.T, greeting string) map[string]string {
	extensions := make(map[string]string)
	lines := strings.Split(greeting, "\n")

	if len(lines) > 1 {
		iLast := len(lines) - 1
		for i, line := range lines {
			prefix := line[0:4]

			// All but the last extension code prefix should be "250-".
			if i != iLast && prefix != "250-" {
				t.Errorf("Extension code prefix is %s, want '250-'", prefix)
			}

			// The last extension code prefix should be "250 ".
			if i == iLast && prefix != "250 " {
				t.Errorf("Extension code prefix is %s, want '250 '", prefix)
			}

			// Skip greeting line.
			if i == 0 {
				continue
			}

			// Add line as extension.
			line = strings.TrimSpace(line[4:]) // Strip code prefix and trailing \r\n
			if idx := strings.Index(line, " "); idx != -1 {
				extensions[line[:idx]] = line[idx+1:]
			} else {
				extensions[line] = ""
			}
		}
	}

	return extensions
}

// Test the extensions listed in response to an EHLO command.
func TestMakeEHLOResponse(t *testing.T) {
	s := &session{}
	s.srv = &Server{}

	// Greeting should be returned without trailing newlines.
	greeting := s.makeEHLOResponse()
	if len(greeting) != len(strings.TrimSpace(greeting)) {
		t.Errorf("EHLO greeting string has leading or trailing whitespace")
	}

	// By default, TLS is not configured, so STARTTLS should not appear.
	extensions := parseExtensions(t, s.makeEHLOResponse())
	if _, ok := extensions["STARTTLS"]; ok {
		t.Errorf("STARTTLS appears in the extension list when TLS is not configured")
	}

	// If TLS is configured, but not already in use, STARTTLS should appear.
	s.srv.TLSConfig = &tls.Config{}
	extensions = parseExtensions(t, s.makeEHLOResponse())
	if _, ok := extensions["STARTTLS"]; !ok {
		t.Errorf("STARTTLS does not appear in the extension list when TLS is configured")
	}

	// If TLS is already used on the connection, STARTTLS should not appear.
	s.tls = true
	extensions = parseExtensions(t, s.makeEHLOResponse())
	if _, ok := extensions["STARTTLS"]; ok {
		t.Errorf("STARTTLS appears in the extension list when TLS is already in use")
	}

	// Verify default SIZE extension is zero.
	s.srv = &Server{}
	extensions = parseExtensions(t, s.makeEHLOResponse())
	if _, ok := extensions["SIZE"]; !ok {
		t.Errorf("SIZE does not appear in the extension list")
	} else if extensions["SIZE"] != "0" {
		t.Errorf("SIZE appears in the extension list with incorrect parameter %s, want %s", extensions["SIZE"], "0")
	}

	// Verify configured maximum message size is listed correctly.
	// Any integer will suffice, as long as it's not hardcoded.
	maxSize := 10 + time.Now().Minute()
	maxSizeStr := fmt.Sprintf("%d", maxSize)
	s.srv = &Server{MaxSize: maxSize}
	extensions = parseExtensions(t, s.makeEHLOResponse())
	if _, ok := extensions["SIZE"]; !ok {
		t.Errorf("SIZE does not appear in the extension list")
	} else if extensions["SIZE"] != maxSizeStr {
		t.Errorf("SIZE appears in the extension list with incorrect parameter %s, want %s", extensions["SIZE"], maxSizeStr)
	}

	// With no authentication handler configured, AUTH should not be advertised.
	s.srv = &Server{}
	extensions = parseExtensions(t, s.makeEHLOResponse())
	if _, ok := extensions["AUTH"]; ok {
		t.Errorf("AUTH appears in the extension list")
	}
}

func createTmpFile(content string) (file *os.File, err error) {
	file, err = ioutil.TempFile("", "")
	if err != nil {
		return
	}
	_, err = file.Write([]byte(content))
	if err != nil {
		return
	}
	err = file.Close()
	return
}

func createTLSFiles() (
	certFile *os.File,
	keyFile *os.File,
	passphrase string,
	err error,
) {
	const certPEM = `-----BEGIN CERTIFICATE-----
MIIDRzCCAi+gAwIBAgIJAKtg4oViVwv4MA0GCSqGSIb3DQEBCwUAMBQxEjAQBgNV
BAMMCWxvY2FsaG9zdDAgFw0xODA0MjAxMzMxNTBaGA8yMDg2MDUwODEzMzE1MFow
FDESMBAGA1UEAwwJbG9jYWxob3N0MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIB
CgKCAQEA8h7vl0gUquis5jRtcnETyD+8WITZO0s53aIzp0Y+9HXiHW6FGJjbOZjM
IvozNVni+83QWKumRTgeSzIIW2j4V8iFMSNrvWmhmCKloesXS1aY6H979e01Ve8J
WAJFRe6vZJd6gC6Z/P+ELU3ie4Vtr1GYfkV7nZ6VFp5/V/5nxGFag5TUlpP5hcoS
9r2kvXofosVwe3x3udT8SEbv5eBD4bKeVyJs/RLbxSuiU1358Y1cDdVuHjcvfm3c
ajhheQ4vX9WXsk7LGGhnf1SrrPN/y+IDTXfvoHn+nJh4vMAB4yzQdE1V1N1AB8RA
0yBVJ6dwxRrSg4BFrNWhj3gfsvrA7wIDAQABo4GZMIGWMB0GA1UdDgQWBBQ4/ncp
befFuKH1hoYkPqLwuRrPRjAfBgNVHSMEGDAWgBQ4/ncpbefFuKH1hoYkPqLwuRrP
RjAJBgNVHRMEAjAAMBEGCWCGSAGG+EIBAQQEAwIGQDALBgNVHQ8EBAMCBaAwEwYD
VR0lBAwwCgYIKwYBBQUHAwEwFAYDVR0RBA0wC4IJbG9jYWxob3N0MA0GCSqGSIb3
DQEBCwUAA4IBAQBJBetEXiEIzKAEpXGX87j6aUON51Fdf6BiLMCghuGKyhnaOG32
4KJhtvVoS3ZUKPylh9c2VdItYlhWp76zd7YKk+3xUOixWeTMQHIvCvRGTyFibOPT
mApwp2pEnJCe4vjUrBaRhiyI+xnB70cWVF2qeernlLUeJA1mfYyQLz+v06ebDWOL
c/hPVQFB94lEdiyjGO7RZfIe8KwcK48g7iv0LQU4+c9MoWM2ZsVM1AL2tHzokSeA
u64gDTW4K0Tzx1ab7KmOFXYUjbz/xWuReMt33EwDXAErKCjbVt2T55Qx8UoKzSh1
tY0KDHdnYOzgsm2HIj2xcJqbeylYQvckNnoC
-----END CERTIFICATE-----`

	const keyPEM = `-----BEGIN RSA PRIVATE KEY-----
Proc-Type: 4,ENCRYPTED
DEK-Info: AES-256-CBC,C16BF8745B2CDB53AC2B1D7609893AA0

O13z7Yq7butaJmMfg9wRis9YnIDPsp4coYI6Ud+JGcP7iXoy95QMhovKWx25o1ol
tvUTsrsG27fHGf9qG02KizApIVtO9c1e0swCWzFrKRQX0JDiZDmilb9xosBNNst1
BOzOTRZEwFGSOCKZRBfSXyqC93TvLJ3DO9IUnKIeGt7upipvg29b/Dur/fyCy2WV
bLHXwUTDBm7j49yfoEyGkDjoB2QO9wgcgbacbnQJQ25fTFUwZpZJEJv6o1tRhoYM
ZMOhC9x1URmdHKN1+z2y5BrB6oNpParfeAMEvs/9FE6jJwYUR28Ql6Mhphfvr9W2
5Gxd3J65Ao9Vi2I5j5X6aBuNjyhXN3ScLjPG4lVZm9RU/uTPEt81pig/d5nSAjvF
Nfc08NuG3cnMyJSE/xScJ4D+GtX8U969wO4oKPCR4E/NFyXPR730ppupDFG6hzPD
PDmiszDtU438JAZ8AuFa1LkbyFnEW6KVD4h7VRr8YDjirCqnkgjNSI6dFY0NQ8H7
SyexB0lrceX6HZc+oNdAtkX3tYdzY3ExzUM5lSF1dkldnRbApLbqc4uuNIVXhXFM
dJnoPdKAzM6i+2EeVUxWNdafKDxnjVSHIHzHfIFJLQ4GS5rnz9keRFdyDjQL07tT
Lu9pPOmsadDXp7oSa81RgoCUfNZeR4jKpCk2BOft0L6ZSqwYFLcQHLIfJaGfn902
TUOTxHt0KzEUYeYSrXC2a6cyvXAd1YI7lOgy60qG89VHyCc2v5Bs4c4FNUDC/+Dj
4ZwogaAbSNkLaE0q3sYQRPdxSqLftyX0KitAgE7oGtdzBfe1cdBoozw3U67NEMMT
6qvk5j7RepPRSrapHtK5pMMdg5XpKFWcOXZ26VHVrDCj4JKdjVb4iyiQi94VveV0
w9+KcOtyrM7/jbQlCWnXpsIkP8VA/RIgh7CBn/h4oF1sO8ywP25OGQ7VWAVq1R9D
8bl8GzIdR9PZpFyOxuIac4rPa8tkDeoXKs4cxoao7H/OZO9o9aTB7CJMTL9yv0Kb
ntWuYxQchE6syoGsOgdGyZhaw4JeFkasDUP5beyNY+278NkzgGTOIMMTXIX46woP
ehzHKGHXVGf7ZiSFF+zAHMXZRSwNVMkOYwlIoRg1IbvIRbAXqAR6xXQTCVzNG0SU
cskojycBca1Cz3hDVIKYZd9beDhprVdr2a4K2nft2g2xRNjKPopsaqXx+VPibFUx
X7542eQ3eAlhkWUuXvt0q5a9WJdjJp9ODA0/d0akF6JQlEHIAyLfoUKB1HYwgUGG
6uRm651FDAab9U4cVC5PY1hfv/QwzpkNDkzgJAZ5SMOfZhq7IdBcqGd3lzPmq2FP
Vy1LVZIl3eM+9uJx5TLsBHH6NhMwtNhFCNa/5ksodQYlTvR8IrrgWlYg4EL69vjS
yt6HhhEN3lFCWvrQXQMp93UklbTlpVt6qcDXiC7HYbs3+EINargRd5Z+xL5i5vkN
f9k7s0xqhloWNPZcyOXMrox8L81WOY+sP4mVlGcfDRLdEJ8X2ofJpOAcwYCnjsKd
uEGsi+l2fTj/F+eZLE6sYoMprgJrbfeqtRWFguUgTn7s5hfU0tZ46al5d0vz8fWK
-----END RSA PRIVATE KEY-----`

	passphrase = "test"

	certFile, err = createTmpFile(certPEM)
	if err != nil {
		return
	}
	keyFile, err = createTmpFile(keyPEM)
	return
}

func TestConfigureTLSWithPassphrase(t *testing.T) {
	certFile, keyFile, passphrase, err := createTLSFiles()
	if err != nil {
		t.Errorf("Unexpected TLS files creation error: %s", err)
		return
	}
	defer func() {
		os.Remove(certFile.Name())
		os.Remove(keyFile.Name())
	}()
	srv := &Server{}
	err = srv.ConfigureTLSWithPassphrase(
		certFile.Name(),
		keyFile.Name(),
		passphrase,
	)
	if err != nil {
		t.Errorf("Unexpected error: %s", err)
	}
	if srv.TLSConfig == nil {
		t.Errorf("Unexpected empty TLS config.")
	}
}

// Benchmark the mail handling without the network stack introducing latency.
func BenchmarkReceive(b *testing.B) {
	server := &Server{} // Default server configuration.

	sendRecv := func(client *textproto.Conn, send string, code int) {
		err := client.PrintfLine(send)
		if err != nil {
			b.Fatal(err)
		}

		_, _, err = client.ReadResponse(code)

		if err != nil {
			// err = errors.Wrap(err, fmt.Sprintf("sent: %q: want: %d, got", send, code))
			b.Fatal(err)
		}
	}

	b.ResetTimer()

	// Benchmark a full mail transaction.
	for i := 0; i < b.N; i++ {

		clientConn, serverConn := net.Pipe()
		session := server.newSession(serverConn)
		go session.serve()

		reader := textproto.NewConn(clientConn)
		_, _ = reader.ReadLine() // Read greeting message first.

		client := textproto.NewConn(clientConn)

		sendRecv(client, "HELO host.example.com", 250)
		sendRecv(client, "MAIL FROM:<sender@example.com>", 250)
		sendRecv(client, "RCPT TO:<recipient@example.com>", 250)
		sendRecv(client, "RCPT TO:", 501)
		sendRecv(client, "DATA", 354)
		sendRecv(client, mimeHeaders+"Test message.\r\n.", 250)
		sendRecv(client, "QUIT", 221)
	}
}
