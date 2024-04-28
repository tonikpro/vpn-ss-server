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

package main

import (
	"container/list"
	"context"
	"flag"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/tonikpro/outline-ss-server/repository"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Jigsaw-Code/outline-sdk/transport"
	"github.com/Jigsaw-Code/outline-sdk/transport/shadowsocks"
	"github.com/op/go-logging"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/tonikpro/outline-ss-server/ipinfo"
	"github.com/tonikpro/outline-ss-server/service"
	"golang.org/x/term"
)

var logger *logging.Logger

// Set by goreleaser default ldflags. See https://goreleaser.com/customization/build/
var version = "dev"

// 59 seconds is most common timeout for servers that do not respond to invalid requests
const tcpReadTimeout time.Duration = 59 * time.Second

// A UDP NAT timeout of at least 5 minutes is recommended in RFC 4787 Section 4.3.
const defaultNatTimeout time.Duration = 5 * time.Minute

func init() {
	var prefix = "%{level:.1s}%{time:2006-01-02T15:04:05.000Z07:00} %{pid} %{shortfile}]"
	if term.IsTerminal(int(os.Stderr.Fd())) {
		// Add color only if the output is the terminal
		prefix = strings.Join([]string{"%{color}", prefix, "%{color:reset}"}, "")
	}
	logging.SetFormatter(logging.MustStringFormatter(strings.Join([]string{prefix, " %{message}"}, "")))
	logging.SetBackend(logging.NewLogBackend(os.Stderr, "", 0))
	logger = logging.MustGetLogger("")
}

type Repository interface {
	LoadConfigs(ctx context.Context, lastID int64) ([]repository.Config, error)
}

type ssPort struct {
	tcpListener *net.TCPListener
	packetConn  net.PacketConn
	cipherList  service.CipherList
}

type SSServer struct {
	natTimeout       time.Duration
	m                *outlineMetrics
	replayCache      service.ReplayCache
	port             *ssPort
	repo             Repository
	lastLoadConfigID int64
}

func (s *SSServer) startPort(portNum int) error {
	listener, err := net.ListenTCP("tcp", &net.TCPAddr{Port: portNum})
	if err != nil {
		//lint:ignore ST1005 Shadowsocks is capitalized.
		return fmt.Errorf("Shadowsocks TCP service failed to start on port %v: %w", portNum, err)
	}
	logger.Infof("Shadowsocks TCP service listening on %v", listener.Addr().String())
	packetConn, err := net.ListenUDP("udp", &net.UDPAddr{Port: portNum})
	if err != nil {
		//lint:ignore ST1005 Shadowsocks is capitalized.
		return fmt.Errorf("Shadowsocks UDP service failed to start on port %v: %w", portNum, err)
	}
	logger.Infof("Shadowsocks UDP service listening on %v", packetConn.LocalAddr().String())
	port := &ssPort{tcpListener: listener, packetConn: packetConn, cipherList: service.NewCipherList()}
	authFunc := service.NewShadowsocksStreamAuthenticator(port.cipherList, &s.replayCache, s.m)
	// TODO: Register initial data metrics at zero.
	tcpHandler := service.NewTCPHandler(portNum, authFunc, s.m, tcpReadTimeout)
	packetHandler := service.NewPacketHandler(s.natTimeout, port.cipherList, s.m)
	s.port = port
	accept := func() (transport.StreamConn, error) {
		conn, err := listener.AcceptTCP()
		if err == nil {
			conn.SetKeepAlive(true)
		}
		return conn, err
	}
	go service.StreamServe(accept, tcpHandler.Handle)
	go packetHandler.Handle(port.packetConn)
	return nil
}

func (s *SSServer) loadConfig() error {
	configs, err := s.repo.LoadConfigs(context.Background(), s.lastLoadConfigID)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	cipherList := list.New() // Values are *List of *CipherEntry.
	for _, keyConfig := range configs {
		cryptoKey, err := shadowsocks.NewEncryptionKey(keyConfig.Cipher, keyConfig.Secret)
		if err != nil {
			return fmt.Errorf("failed to create encyption key for key %v: %w", keyConfig.ID, err)
		}
		entry := service.MakeCipherEntry(fmt.Sprintf("user-%d", keyConfig.ID), cryptoKey, keyConfig.Secret)
		cipherList.PushBack(&entry)

		if s.lastLoadConfigID < keyConfig.ID {
			s.lastLoadConfigID = keyConfig.ID
		}
	}

	s.port.cipherList.Update(cipherList)

	logger.Infof("Loaded %v access keys", len(configs))
	s.m.SetNumAccessKeys(len(configs), 1)

	return nil
}

// Stop serving.
func (s *SSServer) Stop() error {
	tcpErr := s.port.tcpListener.Close()
	udpErr := s.port.packetConn.Close()
	if tcpErr != nil {
		//lint:ignore ST1005 Shadowsocks is capitalized.
		return fmt.Errorf("Shadowsocks TCP service failed to stop: %w", tcpErr)
	}
	logger.Infof("Shadowsocks TCP service stopped")
	if udpErr != nil {
		//lint:ignore ST1005 Shadowsocks is capitalized.
		return fmt.Errorf("Shadowsocks UDP service failed to stop: %w", udpErr)
	}
	logger.Infof("Shadowsocks UDP service stopped")

	return nil
}

// RunSSServer starts a shadowsocks server running, and returns the server or an error.
func RunSSServer(connectionString string, portNum int, natTimeout time.Duration, sm *outlineMetrics, replayHistory int) (*SSServer, error) {
	ctx := context.TODO()
	conn, err := pgx.Connect(ctx, connectionString)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}
	defer conn.Close(ctx)

	server := &SSServer{
		natTimeout:  natTimeout,
		m:           sm,
		replayCache: service.NewReplayCache(replayHistory),
		repo:        repository.NewRepository(conn),
	}

	if err := server.startPort(portNum); err != nil {
		return nil, fmt.Errorf("failed to start server: %w", err)
	}

	if err := server.loadConfig(); err != nil {
		return nil, fmt.Errorf("failed configure server: %w", err)
	}

	go func() {
		t := time.NewTicker(5 * time.Second)
		for {
			select {
			case <-t.C:
				logger.Infof("loading config")
				if err := server.loadConfig(); err != nil {
					logger.Errorf("Failed to update server: %v. Server state may be invalid. Fix the error and try the update again", err)
				}
			default:
				return
			}
		}
	}()
	return server, nil
}

func main() {
	var flags struct {
		ConnectionString string
		Port             int
		MetricsAddr      string
		IPCountryDB      string
		IPASNDB          string
		natTimeout       time.Duration
		replayHistory    int
		Verbose          bool
		Version          bool
	}
	flag.StringVar(&flags.ConnectionString, "conn", "", "Database Connection String")
	flag.IntVar(&flags.Port, "port", 9000, "Port to listen on")
	flag.StringVar(&flags.MetricsAddr, "metrics", "", "Address for the Prometheus metrics")
	flag.StringVar(&flags.IPCountryDB, "ip_country_db", "", "Path to the ip-to-country mmdb file")
	flag.StringVar(&flags.IPASNDB, "ip_asn_db", "", "Path to the ip-to-ASN mmdb file")
	flag.DurationVar(&flags.natTimeout, "udptimeout", defaultNatTimeout, "UDP tunnel timeout")
	flag.IntVar(&flags.replayHistory, "replay_history", 0, "Replay buffer size (# of handshakes)")
	flag.BoolVar(&flags.Verbose, "verbose", true, "Enables verbose logging output")
	flag.BoolVar(&flags.Version, "version", false, "The version of the server")

	flag.Parse()

	if flags.Verbose {
		logging.SetLevel(logging.DEBUG, "")
	} else {
		logging.SetLevel(logging.INFO, "")
	}

	if flags.Version {
		fmt.Println(version)
		return
	}

	if flags.ConnectionString == "" {
		flag.Usage()
		return
	}

	if flags.MetricsAddr != "" {
		http.Handle("/metrics", promhttp.Handler())
		go func() {
			logger.Fatalf("Failed to run metrics server: %v. Aborting.", http.ListenAndServe(flags.MetricsAddr, nil))
		}()
		logger.Infof("Prometheus metrics available at http://%v/metrics", flags.MetricsAddr)
	}

	var err error
	if flags.IPCountryDB != "" {
		logger.Infof("Using IP-Country database at %v", flags.IPCountryDB)
	}
	if flags.IPASNDB != "" {
		logger.Infof("Using IP-ASN database at %v", flags.IPASNDB)
	}
	ip2info, err := ipinfo.NewMMDBIPInfoMap(flags.IPCountryDB, flags.IPASNDB)
	if err != nil {
		logger.Fatalf("Could create IP info map: %v. Aborting", err)
	}
	defer ip2info.Close()

	m := newPrometheusOutlineMetrics(ip2info, prometheus.DefaultRegisterer)
	m.SetBuildInfo(version)
	_, err = RunSSServer(flags.ConnectionString, flags.Port, flags.natTimeout, m, flags.replayHistory)
	if err != nil {
		logger.Fatalf("Server failed to start: %v. Aborting", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
}
