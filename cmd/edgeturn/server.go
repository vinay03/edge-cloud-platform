// Copyright 2022 MobiledgeX, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"

	"strings"
	"sync"
	"time"

	"github.com/edgexr/edge-cloud-platform/pkg/cloudcommon"
	"github.com/edgexr/edge-cloud-platform/pkg/cloudcommon/svcnode"
	"github.com/edgexr/edge-cloud-platform/pkg/log"
	edgetls "github.com/edgexr/edge-cloud-platform/pkg/tls"
	"github.com/gorilla/websocket"
	"github.com/segmentio/ksuid"
	"github.com/xtaci/smux"
)

var listenAddr = flag.String("listenAddr", "127.0.0.1:6080", "EdgeTurn listener address")
var proxyAddr = flag.String("proxyAddr", "127.0.0.1:8443", "EdgeTurn Proxy Address")
var region = flag.String("region", "local", "region name")
var debugLevels = flag.String("d", "", fmt.Sprintf("comma separated list of %v", log.DebugLevelStrings))
var testMode = flag.Bool("testMode", false, "Run EdgeTurn in test mode")
var consoleAddr = flag.String("consoleAddr", "", "Address of the UI console using EdgeTurn, required for origin check")

const (
	ShellConnTimeout   = 5 * time.Minute
	ConsoleConnTimeout = 20 * time.Minute
)

type ProxyValue struct {
	InitURL   *url.URL
	CrmConn   net.Conn
	ProxySess *smux.Session
	Connected chan bool
}

type TurnProxyObj struct {
	mux      sync.Mutex
	proxyMap map[string]*ProxyValue
}

func (cp *TurnProxyObj) Add(token string, proxyVal *ProxyValue) {
	if proxyVal == nil {
		return
	}

	cp.mux.Lock()
	defer cp.mux.Unlock()
	if len(cp.proxyMap) == 0 {
		cp.proxyMap = make(map[string]*ProxyValue)
	}
	cp.proxyMap[token] = proxyVal
}

func (cp *TurnProxyObj) Remove(token string) {
	cp.mux.Lock()
	defer cp.mux.Unlock()
	delete(cp.proxyMap, token)
}

func (cp *TurnProxyObj) Get(token string) *ProxyValue {
	cp.mux.Lock()
	defer cp.mux.Unlock()
	if out, ok := cp.proxyMap[token]; ok {
		return out
	}
	return nil
}

var (
	sigChan   chan os.Signal
	TurnProxy = &TurnProxyObj{}
	nodeMgr   svcnode.SvcNodeMgr
)

func main() {
	nodeMgr.InitFlags()
	flag.Parse()
	log.SetDebugLevelStrs(*debugLevels)

	sigChan = make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt)

	ctx, span, err := nodeMgr.Init(svcnode.SvcNodeTypeEdgeTurn, svcnode.CertIssuerRegional, svcnode.WithRegion(*region))
	if err != nil {
		span.Finish()
		log.FatalLog("Failed to init node", "err", err)
	}
	defer nodeMgr.Finish()

	if *listenAddr == "" {
		log.FatalLog("listenAddr is empty")
	}
	turnLis, err := setupTurnServer(ctx)
	if err != nil {
		log.FatalLog(err.Error())
	}
	defer turnLis.Close()

	log.SpanLog(ctx, log.DebugLevelInfo, "started edgeturn server")

	proxyMux := http.NewServeMux()
	if *proxyAddr == "" {
		log.FatalLog("proxyAddr is empty")
	}
	proxyServer, err := setupProxyServer(ctx, proxyMux)
	if err != nil {
		log.FatalLog(err.Error())
	}
	defer proxyServer.Shutdown(context.Background())

	log.SpanLog(ctx, log.DebugLevelInfo, "started edgeturn proxy server")
	span.Finish()

	<-sigChan
}

func setupTurnServer(ctx context.Context) (net.Listener, error) {
	tlsConfig, err := nodeMgr.InternalPki.GetServerTlsConfig(ctx,
		nodeMgr.CommonNamePrefix(),
		svcnode.CertIssuerRegional,
		[]svcnode.MatchCA{
			svcnode.SameRegionalCloudletMatchCA(),
		})
	if err != nil {
		return nil, fmt.Errorf("failed to get tls config: %v", err)
	}
	if *testMode && tlsConfig == nil {
		tlsConfig, err = edgetls.GetLocalTLSConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to get tls config: %v", err)
		}
	}
	turnConn, err := tls.Listen("tcp", *listenAddr, tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to start server, %v", err)
	}

	go func() {
		for {
			crmConn, err := turnConn.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					break
				}
				log.FatalLog(fmt.Sprintf("failed to accept connection, %v", err))
			}
			go handleConnection(crmConn)
		}
	}()
	return turnConn, nil
}

// On every connection from CRM to EdgeTurn server, it returns a new Access Token.
// This token is used to proxy client connections to actual CRM connection
func handleConnection(crmConn net.Conn) {
	span := log.StartSpan(log.DebugLevelInfo, "handleConnection")
	ctx := log.ContextWithSpan(context.Background(), span)
	defer span.Finish()

	// Fetch exec req info
	var execReqInfo cloudcommon.ExecReqInfo
	d := json.NewDecoder(crmConn)
	err := d.Decode(&execReqInfo)
	if err != nil {
		log.SpanLog(ctx, log.DebugLevelInfo, "failed to decode execreq info", "err", err)
		return
	}
	log.SpanLog(ctx, log.DebugLevelInfo, "received execreq info", "info", execReqInfo)

	// Generate session token
	tokObj := ksuid.New()
	token := tokObj.String()
	proxyVal := &ProxyValue{
		InitURL:   execReqInfo.InitURL,
		CrmConn:   crmConn,
		Connected: make(chan bool),
	}

	// Send Initial Information about the connection
	sessInfo := cloudcommon.SessionInfo{
		Token: token,
	}
	out, err := json.Marshal(&sessInfo)
	if err != nil {
		log.SpanLog(ctx, log.DebugLevelInfo, "failed to marshal session info", "info", sessInfo, "err", err)
		return
	}
	// set up proxy session before writing back reply, otherwise there is a
	// race condition where the client may connect and find the proxy but without
	// the session set yet.
	if execReqInfo.Type == cloudcommon.ExecReqConsole {
		sess, err := smux.Client(crmConn, nil)
		if err != nil {
			log.SpanLog(ctx, log.DebugLevelInfo, "failed to setup smux client", "err", err)
			return
		}
		proxyVal.ProxySess = sess
	}
	TurnProxy.Add(token, proxyVal)

	log.SpanLog(ctx, log.DebugLevelInfo, "send session info", "info", string(out))
	crmConn.Write(out)

	switch execReqInfo.Type {
	case cloudcommon.ExecReqShell:
		select {
		case <-proxyVal.Connected:
			// Once client connects, proxy server will handle closing this
			// connection once the client closes it on its end
		case <-time.After(ShellConnTimeout):
			// Server waits for timeout for client to connect, after which it
			// clears the connection & token
			log.SpanLog(ctx, log.DebugLevelInfo, "timeout waiting for server to accept connection")
			crmConn.Close()
			TurnProxy.Remove(token)
			return

		}
	case cloudcommon.ExecReqConsole:
		select {
		// Note: we can't figure out when to close this connection as there can be multiple requests from
		// single console url and hence we keep the URL valid for a certain time period (ConsoleConnTimeout)
		case <-time.After(ConsoleConnTimeout):
			log.SpanLog(ctx, log.DebugLevelInfo, "closing console connection, user must reconnect with new token")
			crmConn.Close()
			TurnProxy.Remove(token)
		}

	}
}

// Below code from "Start" to "End" is copied from:
//   https://github.com/golang/go/blob/master/src/net/http/transport.go
// It is required for Websockets to work

// Start

type readWriteCloserBody struct {
	br *bufio.Reader // used until empty
	io.ReadWriteCloser
}

func newReadWriteCloserBody(br *bufio.Reader, rwc io.ReadWriteCloser) io.ReadWriteCloser {
	body := &readWriteCloserBody{ReadWriteCloser: rwc}
	if br.Buffered() != 0 {
		body.br = br
	}
	return body
}

// End

type HttpTransport http.Transport

func (t *HttpTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	token := ""
	queryArgs := r.URL.Query()
	tokenVals, ok := queryArgs["edgetoken"]
	if !ok || len(tokenVals) != 1 {
		// try token from cookies
		for _, cookie := range r.Cookies() {
			if cookie.Name == "edgetoken" {
				token = cookie.Value
				break
			}
		}
	} else {
		token = tokenVals[0]
	}
	if token == "" {
		return nil, fmt.Errorf("token %s not found", token)
	}

	proxyVal := TurnProxy.Get(token)
	if proxyVal == nil {
		return nil, fmt.Errorf("missing proxy value for token %s", token)
	}
	if proxyVal.ProxySess == nil {
		TurnProxy.Remove(token)
		return nil, fmt.Errorf("missing session in proxy value for token %s", token)
	}
	if proxyVal.InitURL != nil && proxyVal.InitURL.Scheme != "" {
		r.Header.Set("X-Forwarded-Proto", proxyVal.InitURL.Scheme)
	}

	stream, err := proxyVal.ProxySess.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("failed to open smux stream: %v", err)
	}
	err = r.Write(stream)
	if err != nil {
		return nil, fmt.Errorf("failed to write to smux stream: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(stream), r)
	if err != nil {
		return nil, fmt.Errorf("failed to read response from smux stream: %v", err)
	}
	if resp.StatusCode == http.StatusSwitchingProtocols {
		resp.Body = newReadWriteCloserBody(bufio.NewReader(stream), stream)

	}
	return resp, nil
}

func setupProxyServer(ctx context.Context, serveMux *http.ServeMux) (*http.Server, error) {
	consoleHostname := ""
	if *consoleAddr != "" {
		addr := *consoleAddr
		if !strings.HasPrefix(addr, "http") {
			addr = "http://" + addr
		}
		u, err := url.Parse(addr)
		if err != nil {
			return nil, fmt.Errorf("Invalid consoleAddr, must be a valid URL")
		}
		consoleHostname = u.Host
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {},
		Transport: &HttpTransport{
			Proxy: http.ProxyFromEnvironment,
			Dial: (&net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}).Dial,
			TLSHandshakeTimeout: 10 * time.Second,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
		},
	}

	serveMux.HandleFunc("/", proxy.ServeHTTP)

	serveMux.HandleFunc("/edgeconsole", func(w http.ResponseWriter, r *http.Request) {
		queryArgs := r.URL.Query()
		tokenVals, ok := queryArgs["edgetoken"]
		if !ok || len(tokenVals) != 1 {
			log.SpanLog(ctx, log.DebugLevelInfo, "no token found", "queryArgs", queryArgs)
			r.Close = true
			return
		}
		token := tokenVals[0]
		expire := time.Now().Add(10 * time.Minute)
		cookie := http.Cookie{
			Name:    "edgetoken",
			Value:   token,
			Expires: expire,
		}
		http.SetCookie(w, &cookie)
		log.SpanLog(ctx, log.DebugLevelInfo, "setup console proxy cookies", "path", r.URL)

		proxyVal := TurnProxy.Get(token)
		if proxyVal == nil || proxyVal.InitURL == nil {
			log.SpanLog(ctx, log.DebugLevelInfo, "no proxy value found for token", "token", token)
			r.Close = true
			TurnProxy.Remove(token)
			return
		}
		// This endpoint is used to set (edgetoken) cookie value
		// It redirects to actual console path
		targetURL := proxyVal.InitURL
		target := "https://" + r.Host + targetURL.Path
		if len(targetURL.RawQuery) > 0 {
			target += "?" + targetURL.RawQuery
		}

		// targetURL may contain a cookie embedded in query params encoded in base64  Retrieve any such cookie
		targetQueryParms := targetURL.Query()
		cookieVals, ok := targetQueryParms["sessioncookie"]
		if ok && len(cookieVals) == 1 {
			log.SpanLog(ctx, log.DebugLevelInfo, "found sessioncookie", "queryArgs", queryArgs)
			cookie64 := cookieVals[0]
			sesscookieparm := "&sessioncookie=" + cookie64
			cookiebytes, err := base64.StdEncoding.DecodeString(cookie64)
			if err != nil {
				log.SpanLog(ctx, log.DebugLevelInfo, "cannot decode cookie", "cookie", cookie64)
				return
			}
			cookie := string(cookiebytes)
			cs := strings.Split(cookie, ";")
			cookieName := ""
			cookieVal := ""
			cookiePath := ""
			for i, c := range cs {
				cvals := strings.Split(c, "=")
				if i == 0 {
					if len(cvals) != 2 {
						log.SpanLog(ctx, log.DebugLevelInfo, "unexpected sessioncookie cookie", "cookie", cookie)
						return
					}
					cookieName = cvals[0]
					cookieVal = cvals[1]
				} else {
					if len(cvals) == 2 && cvals[0] == "Path" {
						cookiePath = cvals[1]
					}
				}
			}
			sessionCookie := http.Cookie{
				Name:  cookieName,
				Value: cookieVal,
				Path:  cookiePath,
			}
			http.SetCookie(w, &sessionCookie)
			log.SpanLog(ctx, log.DebugLevelInfo, "added session cookie", "sessionCookie", sessionCookie)

			//remove from the query parms
			target = strings.ReplaceAll(target, sesscookieparm, "")
		}
		log.SpanLog(ctx, log.DebugLevelInfo, "redirect initial edgeconsole request", "target", target)
		http.Redirect(w, r, target,
			http.StatusPermanentRedirect)
	})

	upgrader := websocket.Upgrader{}
	// Check origin returns true if the origin is not set or is equal to the console addr
	upgrader.CheckOrigin = func(r *http.Request) bool {
		origin := r.Header["Origin"]
		if len(origin) == 0 {
			return true
		}
		u, err := url.Parse(origin[0])
		if err != nil {
			return false
		}
		if consoleHostname == "" {
			return false
		}
		return consoleHostname == u.Host
	}
	serveMux.HandleFunc("/edgeshell", func(w http.ResponseWriter, r *http.Request) {
		queryArgs := r.URL.Query()
		tokenVals, ok := queryArgs["edgetoken"]
		token := ""
		if ok && len(tokenVals) == 1 {
			token = tokenVals[0]
		}
		if token == "" {
			log.SpanLog(ctx, log.DebugLevelInfo, "no token found")
			r.Close = true
			return
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.SpanLog(ctx, log.DebugLevelInfo, "failed to upgrade to websocket", "err", err)
			return
		}
		proxyVal := TurnProxy.Get(token)
		if proxyVal == nil || proxyVal.CrmConn == nil {
			log.SpanLog(ctx, log.DebugLevelInfo, "unable to find proxy connection", "token", token)
			r.Close = true
			return
		}
		crmConn := proxyVal.CrmConn

		log.SpanLog(ctx, log.DebugLevelInfo, "client connected to edgeshell", "token", token)
		proxyVal.Connected <- true
		defer c.Close()

		closeChan := make(chan bool)
		go func() {
			for {
				_, msg, err := c.ReadMessage()
				if err != nil {
					if _, ok := err.(*websocket.CloseError); !ok {
						log.SpanLog(ctx, log.DebugLevelInfo, "failed to read from websocket", "err", err)
					}
					closeChan <- true
					break
				}
				_, err = crmConn.Write(msg)
				if err != nil {
					if err != io.EOF {
						log.SpanLog(ctx, log.DebugLevelInfo, "failed to write to proxyConn", "err", err)
					}
					closeChan <- true
					break
				}
			}
		}()
		go func() {
			for {
				done := false
				buf := make([]byte, 1500)
				n, err := crmConn.Read(buf)
				if err != nil {
					if err != io.EOF && !errors.Is(err, net.ErrClosed) {
						log.SpanLog(ctx, log.DebugLevelInfo, "failed to read from proxyConn", "err", err)
					}
					if n <= 0 {
						closeChan <- true
						break
					}
					done = true
				}

				err = c.WriteMessage(websocket.TextMessage, buf[:n])
				if err != nil {
					if _, ok := err.(*websocket.CloseError); !ok {
						log.SpanLog(ctx, log.DebugLevelInfo, "failed to write to websocket", "err", err)
					}
					closeChan <- true
					break
				}
				if done {
					closeChan <- true
					break
				}
			}
		}()
		<-closeChan
		crmConn.Close()
		TurnProxy.Remove(token)
		log.SpanLog(ctx, log.DebugLevelInfo, "client exited", "token", token)
	})

	server := &http.Server{
		Addr:    *proxyAddr,
		Handler: serveMux,
	}
	if *testMode {
		// In test mode, setup HTTP server with TLS
		var tlsConfig *tls.Config
		tlsConfig, err := edgetls.GetLocalTLSConfig()
		if err != nil {
			return nil, fmt.Errorf("unable to fetch tls local server config, %v", err)
		}
		server.TLSConfig = tlsConfig
	}
	go func() {
		var err error
		if server.TLSConfig != nil {
			err = server.ListenAndServeTLS("", "")
		} else {
			// Certs will be provided by LB
			err = server.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			log.FatalLog("Failed to start console proxy server", "err", err)
		}
	}()
	return server, nil
}
