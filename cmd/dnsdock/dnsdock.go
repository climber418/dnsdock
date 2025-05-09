/* dnsdock.go
 *
 * Copyright (C) 2016 Alexandre ACEBEDO
 *
 * This software may be modified and distributed under the terms
 * of the MIT license.  See the LICENSE file for details.
 */

package main

import (
	"crypto/tls"
	"crypto/x509"
	"github.com/aacebedo/dnsdock/internal/core"
	"github.com/aacebedo/dnsdock/internal/servers"
	"github.com/aacebedo/dnsdock/internal/utils"
	"github.com/op/go-logging"
	"os"
)
// GitSummary contains the version number
var GitSummary string
var logger = logging.MustGetLogger("dnsdock.main")

func main() {
    logger.Info("dnsdock starting...")
	var cmdLine = core.NewCommandLine(GitSummary)
	config, err := cmdLine.ParseParameters(os.Args[1:])
	if err != nil {
		logger.Fatalf(err.Error())
	}
	verbosity := 0
	if !config.Quiet {
		if !config.Verbose {
			verbosity = 1
		} else {
			verbosity = 2
		}
	}
	err = utils.InitLoggers(verbosity)
	if err != nil {
		logger.Fatalf("Unable to initialize loggers! %s", err.Error())
	}

	dnsServer := servers.NewDNSServer(config)

	var tlsConfig *tls.Config
	if config.TlsVerify {
		clientCert, err := tls.LoadX509KeyPair(config.TlsCert, config.TlsKey)
		if err != nil {
			logger.Fatalf("Error: '%s'", err)
		}
		tlsConfig = &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{clientCert},
		}
		pemData, err := os.ReadFile(config.TlsCaCert)
		if err == nil {
			rootCert := x509.NewCertPool()
			rootCert.AppendCertsFromPEM(pemData)
			tlsConfig.RootCAs = rootCert
		} else {
			logger.Fatalf("Error: '%s'", err)
		}
	}

	docker, err := core.NewDockerManager(config, dnsServer, tlsConfig)
	if err != nil {
		logger.Fatalf("Error: '%s'", err)
	}
	if err := docker.Start(); err != nil {
		logger.Fatalf("Error: '%s'", err)
	}

	httpServer := servers.NewHTTPServer(config, dnsServer)
	go func() {
		if err := httpServer.Start(); err != nil {
			logger.Fatalf("Error: '%s'", err)
		}
	}()

	if err := dnsServer.Start(); err != nil {
		logger.Fatalf("Error: '%s'", err)
	}
}
