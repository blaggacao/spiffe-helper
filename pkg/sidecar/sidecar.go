package sidecar

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync/atomic"
	"time"

	"github.com/andres-erbsen/clock"
	proto "github.com/spiffe/go-spiffe/proto/spiffe/workload"
	"github.com/spiffe/spire/api/workload"
	"golang.org/x/sys/unix"
)

// Config contains config variables when creating a SPIFFE Sidecar.
type Config struct {
	AgentAddress          string `hcl:"agentAddress"`
	Cmd                   string `hcl:"cmd"`
	CmdArgs               string `hcl:"cmdArgs"`
	CertDir               string `hcl:"certDir"`
	SvidFileName          string `hcl:"svidFileName"`
	SvidKeyFileName       string `hcl:"svidKeyFileName"`
	SvidBundleFileName    string `hcl:"svidBundleFileName"`
	RenewSignal           string `hcl:"renewSignal"`
	Timeout               string `hcl:"timeout"`
	ReloadExternalProcess func() error
}

// Sidecar is the component that consumes the Workload API and renews certs
// implements the interface Sidecar
type Sidecar struct {
	config            *Config
	processRunning    int32
	process           *os.Process
	workloadAPIClient workload.X509Client
	certReadyChan     chan struct{}
}

const (
	// default timeout Duration for the workloadAPI client when the defaultTimeout
	// is not configured in the .conf file
	defaultTimeout = 5 * time.Second
	delayMin       = time.Second
	delayMax       = time.Minute

	certsFileMode = os.FileMode(0644)
	keyFileMode   = os.FileMode(0600)
)

// NewSidecar creates a new SPIFFE sidecar
func NewSidecar(config *Config) (*Sidecar, error) {
	timeout, err := getTimeout(config)
	if err != nil {
		return nil, err
	}

	return &Sidecar{
		config:            config,
		workloadAPIClient: newWorkloadAPIClient(config.AgentAddress, timeout),
		certReadyChan:     make(chan struct{}, 1),
	}, nil
}

// RunDaemon starts the main loop
// Starts the workload API client to listen for new SVID updates
// When a new SVID is received on the updateChan, the SVID certificates
// are stored in disk and a restart signal is sent to the proxy's process
func (s *Sidecar) RunDaemon(ctx context.Context) error {
	// Create channel for interrupt signal
	errorChan := make(chan error, 1)

	updateChan := s.workloadAPIClient.UpdateChan()

	// start the workloadAPIClient
	go func() {
		clk := clock.New()
		delay := delayMin
		for {
			err := s.workloadAPIClient.Start()
			if err != nil {
				log.Printf("failed: %v; retrying in %s", err, delay)
				timer := clk.Timer(delay)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					errorChan <- ctx.Err()
					return
				}

				delay = time.Duration(float64(delay) * 1.5)
				if delay > delayMax {
					delay = delayMax
				}
			}
		}
	}()
	defer s.workloadAPIClient.Stop()

	for {
		select {
		case svidResponse := <-updateChan:
			updateCertificates(s, svidResponse)
		case err := <-errorChan:
			return err
		case <-ctx.Done():
			return nil
		}
	}
}

// Updates the certificates stored in disk and signal the Process to restart
func updateCertificates(s *Sidecar, svidResponse *proto.X509SVIDResponse) {
	log.Println("Updating certificates")

	err := s.dumpBundles(svidResponse)
	if err != nil {
		log.Println(err.Error())
		return
	}
	err = s.signalProcess()
	if err != nil {
		log.Println(err.Error())
	}

	select {
	case s.certReadyChan <- struct{}{}:
	default:
	}
}

// CertReadyChan returns a channel to know when the certificates are ready
func (s *Sidecar) CertReadyChan() <-chan struct{} {
	return s.certReadyChan
}

// newWorkloadAPIClient creates a workload.X509Client
func newWorkloadAPIClient(agentAddress string, timeout time.Duration) workload.X509Client {
	addr := &net.UnixAddr{
		Net:  "unix",
		Name: agentAddress,
	}
	config := &workload.X509ClientConfig{
		Addr:    addr,
		Timeout: timeout,
	}
	return workload.NewX509Client(config)
}

// signalProcess sends the configured Renew signal to the process running the proxy
// to reload itself so that the proxy uses the new SVID
func (s *Sidecar) signalProcess() (err error) {
	switch s.config.ReloadExternalProcess {
	case nil:
		if atomic.LoadInt32(&s.processRunning) == 0 {
			cmd := exec.Command(s.config.Cmd, strings.Split(s.config.CmdArgs, " ")...) // #nosec
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			err = cmd.Start()
			if err != nil {
				return fmt.Errorf("error executing process: %v\n%v", s.config.Cmd, err)
			}
			s.process = cmd.Process
			go s.checkProcessExit()
		} else {
			// Signal to reload certs
			sig := unix.SignalNum(s.config.RenewSignal)
			if sig == 0 {
				return fmt.Errorf("error getting signal: %v", s.config.RenewSignal)
			}

			err = s.process.Signal(sig)
			if err != nil {
				return fmt.Errorf("error signaling process with signal: %v\n%v", sig, err)
			}
		}

	default:
		err = s.config.ReloadExternalProcess()
		if err != nil {
			return fmt.Errorf("error reloading external process: %v", err)
		}
	}

	return nil
}

func (s *Sidecar) checkProcessExit() {
	atomic.StoreInt32(&s.processRunning, 1)
	_, err := s.process.Wait()
	if err != nil {
		log.Printf("error waiting for process exit: %v", err)
	}

	atomic.StoreInt32(&s.processRunning, 0)
}

// dumpBundles takes a X509SVIDResponse, representing a svid message from
// the Workload API, and calls writeCerts and writeKey to write to disk
// the svid, key and bundle of certificates
func (s *Sidecar) dumpBundles(svidResponse *proto.X509SVIDResponse) error {

	// There may be more than one certificate, but we are interested in the first one only
	svid := svidResponse.Svids[0]

	svidFile := path.Join(s.config.CertDir, s.config.SvidFileName)
	svidKeyFile := path.Join(s.config.CertDir, s.config.SvidKeyFileName)
	svidBundleFile := path.Join(s.config.CertDir, s.config.SvidBundleFileName)

	err := s.writeCerts(svidFile, svid.X509Svid)
	if err != nil {
		return err
	}

	err = s.writeKey(svidKeyFile, svid.X509SvidKey)
	if err != nil {
		return err
	}

	err = s.writeCerts(svidBundleFile, svid.Bundle)
	if err != nil {
		return err
	}

	return nil
}

// writeCerts takes a slice of bytes, which may contain multiple certificates,
// and encodes them as PEM blocks, writing them to file
func (s *Sidecar) writeCerts(file string, data []byte) error {
	certs, err := x509.ParseCertificates(data)
	if err != nil {
		return err
	}

	pemData := []byte{}
	for _, cert := range certs {
		b := &pem.Block{
			Type:  "CERTIFICATE",
			Bytes: cert.Raw,
		}
		pemData = append(pemData, pem.EncodeToMemory(b)...)
	}

	return ioutil.WriteFile(file, pemData, certsFileMode)
}

// writeKey takes a private key as a slice of bytes,
// formats as PEM, and writes it to file
func (s *Sidecar) writeKey(file string, data []byte) error {
	b := &pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: data,
	}

	return ioutil.WriteFile(file, pem.EncodeToMemory(b), keyFileMode)
}

// parses a time.Duration from the the Config,
// if there's an error during parsing, maybe because
// it's not well defined or not defined at all in the
// config, returns the defaultTimeout constant
func getTimeout(config *Config) (time.Duration, error) {
	if config.Timeout == "" {
		return defaultTimeout, nil
	}

	t, err := time.ParseDuration(config.Timeout)
	if err != nil {
		return 0, err
	}
	return t, nil
}