// Copyright 2018 Jigsaw Operations LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package service

import (
	"bytes"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport/shadowsocks"
	logging "github.com/op/go-logging"
	"github.com/shadowsocks/go-shadowsocks2/socks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tonikpro/outline-ss-server/ipinfo"
	onet "github.com/tonikpro/outline-ss-server/net"
)

const timeout = 5 * time.Minute

var clientAddr = net.UDPAddr{IP: []byte{192, 0, 2, 1}, Port: 12345}
var targetAddr = net.UDPAddr{IP: []byte{192, 0, 2, 2}, Port: 54321}
var dnsAddr = net.UDPAddr{IP: []byte{192, 0, 2, 3}, Port: 53}
var natCryptoKey *shadowsocks.EncryptionKey

func init() {
	logging.SetLevel(logging.INFO, "")
	natCryptoKey, _ = shadowsocks.NewEncryptionKey(shadowsocks.CHACHA20IETFPOLY1305, "test password")
}

type packet struct {
	addr    net.Addr
	payload []byte
	err     error
}

type fakePacketConn struct {
	net.PacketConn
	send     chan packet
	recv     chan packet
	deadline time.Time
}

func makePacketConn() *fakePacketConn {
	return &fakePacketConn{
		send: make(chan packet, 1),
		recv: make(chan packet),
	}
}

func (conn *fakePacketConn) SetReadDeadline(deadline time.Time) error {
	conn.deadline = deadline
	return nil
}

func (conn *fakePacketConn) WriteTo(payload []byte, addr net.Addr) (int, error) {
	conn.send <- packet{addr, payload, nil}
	return len(payload), nil
}

func (conn *fakePacketConn) ReadFrom(buffer []byte) (int, net.Addr, error) {
	pkt, ok := <-conn.recv
	if !ok {
		return 0, nil, net.ErrClosed
	}
	n := copy(buffer, pkt.payload)
	if n < len(pkt.payload) {
		return n, pkt.addr, errors.New("buffer was too short")
	}
	return n, pkt.addr, pkt.err
}

func (conn *fakePacketConn) Close() error {
	close(conn.send)
	close(conn.recv)
	return nil
}

type udpReport struct {
	clientInfo                         ipinfo.IPInfo
	accessKey, status                  string
	clientProxyBytes, proxyTargetBytes int
}

// Stub metrics implementation for testing NAT behaviors.
type natTestMetrics struct {
	natEntriesAdded int
	upstreamPackets []udpReport
}

var _ UDPMetrics = (*natTestMetrics)(nil)

func (m *natTestMetrics) GetIPInfo(net.IP) (ipinfo.IPInfo, error) {
	return ipinfo.IPInfo{}, nil
}
func (m *natTestMetrics) AddUDPPacketFromClient(clientInfo ipinfo.IPInfo, accessKey, status string, clientProxyBytes, proxyTargetBytes int) {
	m.upstreamPackets = append(m.upstreamPackets, udpReport{clientInfo, accessKey, status, clientProxyBytes, proxyTargetBytes})
}
func (m *natTestMetrics) AddUDPPacketFromTarget(clientInfo ipinfo.IPInfo, accessKey, status string, targetProxyBytes, proxyClientBytes int) {
}
func (m *natTestMetrics) AddUDPNatEntry(clientAddr net.Addr, accessKey string) {
	m.natEntriesAdded++
}
func (m *natTestMetrics) RemoveUDPNatEntry(clientAddr net.Addr, accessKey string) {
}
func (m *natTestMetrics) AddUDPCipherSearch(accessKeyFound bool, timeToCipher time.Duration) {}

// Takes a validation policy, and returns the metrics it
// generates when localhost access is attempted
func sendToDiscard(payloads [][]byte, validator onet.TargetIPValidator) *natTestMetrics {
	ciphers, _ := MakeTestCiphers([]string{"asdf"})
	cipher := ciphers.SnapshotForClientIP(nil)[0].Value.(*CipherEntry).CryptoKey
	clientConn := makePacketConn()
	metrics := &natTestMetrics{}
	handler := NewPacketHandler(timeout, ciphers, metrics)
	handler.SetTargetIPValidator(validator)
	done := make(chan struct{})
	go func() {
		handler.Handle(clientConn)
		done <- struct{}{}
	}()

	// Send one packet to the "discard" port on localhost
	targetAddr := socks.ParseAddr("127.0.0.1:9")
	for _, payload := range payloads {
		plaintext := append(targetAddr, payload...)
		ciphertext := make([]byte, cipher.SaltSize()+len(plaintext)+cipher.TagSize())
		shadowsocks.Pack(ciphertext, plaintext, cipher)
		clientConn.recv <- packet{
			addr: &net.UDPAddr{
				IP:   net.ParseIP("192.0.2.1"),
				Port: 54321,
			},
			payload: ciphertext,
		}
	}

	clientConn.Close()
	<-done
	return metrics
}

func TestIPFilter(t *testing.T) {
	// Test both the first-packet and subsequent-packet cases.
	payloads := [][]byte{[]byte("payload1"), []byte("payload2")}

	t.Run("Localhost allowed", func(t *testing.T) {
		metrics := sendToDiscard(payloads, allowAll)
		assert.Equal(t, metrics.natEntriesAdded, 1, "Expected 1 NAT entry, not %d", metrics.natEntriesAdded)
	})

	t.Run("Localhost not allowed", func(t *testing.T) {
		metrics := sendToDiscard(payloads, onet.RequirePublicIP)
		assert.Equal(t, 0, metrics.natEntriesAdded, "Unexpected NAT entry on rejected packet")
		assert.Equal(t, 2, len(metrics.upstreamPackets), "Expected 2 reports, not %v", metrics.upstreamPackets)
		for _, report := range metrics.upstreamPackets {
			assert.Greater(t, report.clientProxyBytes, 0, "Expected nonzero input packet size")
			assert.Equal(t, 0, report.proxyTargetBytes, "No bytes should be sent due to a disallowed packet")
			assert.Equal(t, report.accessKey, "id-0", "Unexpected access key: %s", report.accessKey)
		}
	})
}

func TestUpstreamMetrics(t *testing.T) {
	// Test both the first-packet and subsequent-packet cases.
	const N = 10
	payloads := make([][]byte, 0)
	for i := 1; i <= N; i++ {
		payloads = append(payloads, make([]byte, i))
	}

	metrics := sendToDiscard(payloads, allowAll)

	assert.Equal(t, N, len(metrics.upstreamPackets), "Expected %d reports, not %v", N, metrics.upstreamPackets)
	for i, report := range metrics.upstreamPackets {
		assert.Equal(t, i+1, report.proxyTargetBytes, "Expected %d payload bytes, not %d", i+1, report.proxyTargetBytes)
		assert.Greater(t, report.clientProxyBytes, report.proxyTargetBytes, "Expected nonzero input overhead (%d > %d)", report.clientProxyBytes, report.proxyTargetBytes)
		assert.Equal(t, "id-0", report.accessKey, "Unexpected access key name: %s", report.accessKey)
		assert.Equal(t, "OK", report.status, "Wrong status: %s", report.status)
	}
}

func assertAlmostEqual(t *testing.T, a, b time.Time) {
	delta := a.Sub(b)
	limit := 100 * time.Millisecond
	if delta > limit || -delta > limit {
		t.Errorf("Times are not close: %v, %v", a, b)
	}
}

func TestNATEmpty(t *testing.T) {
	nat := newNATmap(timeout, &natTestMetrics{}, &sync.WaitGroup{})
	if nat.Get("foo") != nil {
		t.Error("Expected nil value from empty NAT map")
	}
}

func setupNAT() (*fakePacketConn, *fakePacketConn, *natconn) {
	nat := newNATmap(timeout, &natTestMetrics{}, &sync.WaitGroup{})
	clientConn := makePacketConn()
	targetConn := makePacketConn()
	nat.Add(&clientAddr, clientConn, natCryptoKey, targetConn, ipinfo.IPInfo{CountryCode: "ZZ"}, "key id")
	entry := nat.Get(clientAddr.String())
	return clientConn, targetConn, entry
}

func TestNATGet(t *testing.T) {
	_, targetConn, entry := setupNAT()
	if entry == nil {
		t.Fatal("Failed to find target conn")
	}
	if entry.PacketConn != targetConn {
		t.Error("Mismatched connection returned")
	}
}

func TestNATWrite(t *testing.T) {
	_, targetConn, entry := setupNAT()

	// Simulate one generic packet being sent
	buf := []byte{1}
	entry.WriteTo([]byte{1}, &targetAddr)
	assertAlmostEqual(t, targetConn.deadline, time.Now().Add(timeout))
	sent := <-targetConn.send
	if !bytes.Equal(sent.payload, buf) {
		t.Errorf("Mismatched payload: %v != %v", sent.payload, buf)
	}
	if sent.addr != &targetAddr {
		t.Errorf("Mismatched address: %v != %v", sent.addr, &targetAddr)
	}
}

func TestNATWriteDNS(t *testing.T) {
	_, targetConn, entry := setupNAT()

	// Simulate one DNS query being sent.
	buf := []byte{1}
	entry.WriteTo(buf, &dnsAddr)
	// DNS-only connections have a fixed timeout of 17 seconds.
	assertAlmostEqual(t, targetConn.deadline, time.Now().Add(17*time.Second))
	sent := <-targetConn.send
	if !bytes.Equal(sent.payload, buf) {
		t.Errorf("Mismatched payload: %v != %v", sent.payload, buf)
	}
	if sent.addr != &dnsAddr {
		t.Errorf("Mismatched address: %v != %v", sent.addr, &targetAddr)
	}
}

func TestNATWriteDNSMultiple(t *testing.T) {
	_, targetConn, entry := setupNAT()

	// Simulate three DNS queries being sent.
	buf := []byte{1}
	entry.WriteTo(buf, &dnsAddr)
	<-targetConn.send
	entry.WriteTo(buf, &dnsAddr)
	<-targetConn.send
	entry.WriteTo(buf, &dnsAddr)
	<-targetConn.send
	// DNS-only connections have a fixed timeout of 17 seconds.
	assertAlmostEqual(t, targetConn.deadline, time.Now().Add(17*time.Second))
}

func TestNATWriteMixed(t *testing.T) {
	_, targetConn, entry := setupNAT()

	// Simulate both non-DNS and DNS packets being sent.
	buf := []byte{1}
	entry.WriteTo(buf, &targetAddr)
	<-targetConn.send
	entry.WriteTo(buf, &dnsAddr)
	<-targetConn.send
	// Mixed DNS and non-DNS connections should have the user-specified timeout.
	assertAlmostEqual(t, targetConn.deadline, time.Now().Add(timeout))
}

func TestNATFastClose(t *testing.T) {
	clientConn, targetConn, entry := setupNAT()

	// Send one DNS query.
	query := []byte{1}
	entry.WriteTo(query, &dnsAddr)
	sent := <-targetConn.send
	require.Len(t, sent.payload, 1)
	// Send the response.
	response := []byte{1, 2, 3, 4, 5}
	received := packet{addr: &dnsAddr, payload: response}
	targetConn.recv <- received
	sent, ok := <-clientConn.send
	if !ok {
		t.Error("clientConn was closed")
	}
	if len(sent.payload) <= len(response) {
		t.Error("Packet is too short to be shadowsocks-AEAD")
	}
	if sent.addr != &clientAddr {
		t.Errorf("Address mismatch: %v != %v", sent.addr, clientAddr)
	}

	// targetConn should be scheduled to close immediately.
	assertAlmostEqual(t, targetConn.deadline, time.Now())
}

func TestNATNoFastClose_NotDNS(t *testing.T) {
	clientConn, targetConn, entry := setupNAT()

	// Send one non-DNS packet.
	query := []byte{1}
	entry.WriteTo(query, &targetAddr)
	sent := <-targetConn.send
	require.Len(t, sent.payload, 1)
	// Send the response.
	response := []byte{1, 2, 3, 4, 5}
	received := packet{addr: &targetAddr, payload: response}
	targetConn.recv <- received
	sent, ok := <-clientConn.send
	if !ok {
		t.Error("clientConn was closed")
	}
	if len(sent.payload) <= len(response) {
		t.Error("Packet is too short to be shadowsocks-AEAD")
	}
	if sent.addr != &clientAddr {
		t.Errorf("Address mismatch: %v != %v", sent.addr, clientAddr)
	}
	// targetConn should be scheduled to close after the full timeout.
	assertAlmostEqual(t, targetConn.deadline, time.Now().Add(timeout))
}

func TestNATNoFastClose_MultipleDNS(t *testing.T) {
	clientConn, targetConn, entry := setupNAT()

	// Send two DNS packets.
	query1 := []byte{1}
	entry.WriteTo(query1, &dnsAddr)
	<-targetConn.send
	query2 := []byte{2}
	entry.WriteTo(query2, &dnsAddr)
	<-targetConn.send

	// Send a response.
	response := []byte{1, 2, 3, 4, 5}
	received := packet{addr: &dnsAddr, payload: response}
	targetConn.recv <- received
	<-clientConn.send

	// targetConn should be scheduled to close after the DNS timeout.
	assertAlmostEqual(t, targetConn.deadline, time.Now().Add(17*time.Second))
}

// Implements net.Error
type fakeTimeoutError struct {
	error
}

func (e *fakeTimeoutError) Timeout() bool {
	return true
}

func (e *fakeTimeoutError) Temporary() bool {
	return false
}

func TestNATTimeout(t *testing.T) {
	_, targetConn, entry := setupNAT()

	// Simulate a non-DNS initial packet.
	entry.WriteTo([]byte{1}, &targetAddr)
	<-targetConn.send
	// Simulate a read timeout.
	received := packet{err: &fakeTimeoutError{}}
	before := time.Now()
	targetConn.recv <- received
	// Wait for targetConn to close.
	if _, ok := <-targetConn.send; ok {
		t.Error("targetConn should be closed due to read timeout")
	}
	// targetConn should be closed as soon as the timeout error is received.
	assertAlmostEqual(t, before, time.Now())
}

// Simulates receiving invalid UDP packets on a server with 100 ciphers.
func BenchmarkUDPUnpackFail(b *testing.B) {
	cipherList, err := MakeTestCiphers(makeTestSecrets(100))
	if err != nil {
		b.Fatal(err)
	}
	testPayload := makeTestPayload(50)
	textBuf := make([]byte, serverUDPBufferSize)
	testIP := net.ParseIP("192.0.2.1")
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		findAccessKeyUDP(testIP, textBuf, testPayload, cipherList)
	}
}

// Simulates receiving valid UDP packets from 100 different users, each with
// their own cipher and IP address.
func BenchmarkUDPUnpackRepeat(b *testing.B) {
	const numCiphers = 100 // Must be <256
	cipherList, err := MakeTestCiphers(makeTestSecrets(numCiphers))
	if err != nil {
		b.Fatal(err)
	}
	testBuf := make([]byte, serverUDPBufferSize)
	packets := [numCiphers][]byte{}
	ips := [numCiphers]net.IP{}
	snapshot := cipherList.SnapshotForClientIP(nil)
	for i, element := range snapshot {
		packets[i] = make([]byte, 0, serverUDPBufferSize)
		plaintext := makeTestPayload(50)
		packets[i], err = shadowsocks.Pack(make([]byte, serverUDPBufferSize), plaintext, element.Value.(*CipherEntry).CryptoKey)
		if err != nil {
			b.Error(err)
		}
		ips[i] = net.IPv4(192, 0, 2, byte(i))
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		cipherNumber := n % numCiphers
		ip := ips[cipherNumber]
		packet := packets[cipherNumber]
		_, _, _, err := findAccessKeyUDP(ip, testBuf, packet, cipherList)
		if err != nil {
			b.Error(err)
		}
	}
}

// Simulates receiving valid UDP packets from 100 different IP addresses,
// all using the same cipher.
func BenchmarkUDPUnpackSharedKey(b *testing.B) {
	cipherList, err := MakeTestCiphers(makeTestSecrets(1)) // One widely shared key
	if err != nil {
		b.Fatal(err)
	}
	testBuf := make([]byte, serverUDPBufferSize)
	plaintext := makeTestPayload(50)
	snapshot := cipherList.SnapshotForClientIP(nil)
	cryptoKey := snapshot[0].Value.(*CipherEntry).CryptoKey
	packet, err := shadowsocks.Pack(make([]byte, serverUDPBufferSize), plaintext, cryptoKey)
	require.Nil(b, err)

	const numIPs = 100 // Must be <256
	ips := [numIPs]net.IP{}
	for i := 0; i < numIPs; i++ {
		ips[i] = net.IPv4(192, 0, 2, byte(i))
	}
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		ip := ips[n%numIPs]
		_, _, _, err := findAccessKeyUDP(ip, testBuf, packet, cipherList)
		if err != nil {
			b.Error(err)
		}
	}
}

func TestUDPEarlyClose(t *testing.T) {
	cipherList, err := MakeTestCiphers(makeTestSecrets(1))
	if err != nil {
		t.Fatal(err)
	}
	testMetrics := &natTestMetrics{}
	const testTimeout = 200 * time.Millisecond
	s := NewPacketHandler(testTimeout, cipherList, testMetrics)

	clientConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("ListenUDP failed: %v", err)
	}
	require.Nil(t, clientConn.Close())
	// This should return quickly without timing out.
	s.Handle(clientConn)
}

// Makes sure the UDP listener returns [io.ErrClosed] on reads and writes after Close().
func TestClosedUDPListenerError(t *testing.T) {
	var packetConn net.PacketConn
	packetConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	require.NoError(t, err)
	err = packetConn.Close()
	require.NoError(t, err)

	_, _, err = packetConn.ReadFrom(nil)
	require.ErrorIs(t, err, net.ErrClosed)

	_, err = packetConn.WriteTo(nil, &net.UDPAddr{})
	require.ErrorIs(t, err, net.ErrClosed)
}
