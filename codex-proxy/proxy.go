package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const (
	loginRequestID     = "edka-codex-subscription-login"
	initializeTimeout  = 30 * time.Second
	websocketReadLimit = 16 << 20
)

type Settings struct {
	AuthMode          AuthMode
	Addr              string
	Upstream          string
	BrokerURL         string
	BrokerTokenFile   string
	ProviderAddr      string
	ProviderTokenFile string
	SharedSecretFile  string
	Issuer            string
	Audience          string
	Broker            TokenBroker
}

func (s Settings) Validate() error {
	authMode := s.effectiveAuthMode()
	if !authMode.Valid() {
		return fmt.Errorf("unsupported authentication mode %q", authMode)
	}
	if s.Addr == "" {
		return fmt.Errorf("proxy listen address is required")
	}
	upstream, err := url.Parse(s.Upstream)
	if err != nil || (upstream.Scheme != "ws" && upstream.Scheme != "wss") || upstream.Host == "" {
		return fmt.Errorf("valid WebSocket upstream is required")
	}
	if authMode == AuthModeSubscription {
		if s.Broker == nil {
			if s.BrokerURL == "" {
				return fmt.Errorf("broker URL is required")
			}
			if s.BrokerTokenFile == "" {
				return fmt.Errorf("broker token file is required")
			}
		}
	} else {
		if err := validateLoopbackAddress(s.ProviderAddr); err != nil {
			return err
		}
		if s.ProviderTokenFile == "" {
			return fmt.Errorf("provider token file is required")
		}
	}
	if s.SharedSecretFile == "" {
		return fmt.Errorf("WebSocket shared-secret file is required")
	}
	if s.Issuer == "" {
		return fmt.Errorf("WebSocket token issuer is required")
	}
	if s.Audience == "" {
		return fmt.Errorf("WebSocket token audience is required")
	}
	return nil
}

func (s Settings) effectiveAuthMode() AuthMode {
	if s.AuthMode == "" {
		return AuthModeSubscription
	}
	return s.AuthMode
}

func validateLoopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("valid provider listen address is required: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("provider listen address must use a loopback IP")
	}
	return nil
}

type Proxy struct {
	settings   Settings
	logger     *slog.Logger
	broker     *resilientBroker
	provider   http.Handler
	authorizer remoteAuthorizer
}

func New(settings Settings, logger *slog.Logger) *Proxy {
	authMode := settings.effectiveAuthMode()
	var broker *resilientBroker
	if authMode == AuthModeSubscription {
		upstream := settings.Broker
		if upstream == nil {
			upstream = HTTPBroker{
				URL:       settings.BrokerURL,
				TokenFile: settings.BrokerTokenFile,
			}
		}
		broker = newResilientBroker(upstream)
	}
	var provider http.Handler
	if authMode.IsAPIKey() {
		provider = newProviderProxy(
			providerUpstream(authMode),
			settings.ProviderTokenFile,
			nil,
			logger,
		)
	}
	return &Proxy{
		settings: settings,
		logger:   logger,
		broker:   broker,
		provider: provider,
		authorizer: remoteAuthorizer{
			sharedSecretFile: settings.SharedSecretFile,
			issuer:           settings.Issuer,
			audience:         settings.Audience,
		},
	}
}

func (p *Proxy) Serve(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/", p.handle)
	servers := []*http.Server{{
		Addr:              p.settings.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    32 << 10,
	}}
	if p.provider != nil {
		servers = append(servers, &http.Server{
			Addr:              p.settings.ProviderAddr,
			Handler:           p.provider,
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       2 * time.Minute,
			MaxHeaderBytes:    32 << 10,
		})
	}

	errc := make(chan error, len(servers))
	for _, server := range servers {
		go func() { errc <- server.ListenAndServe() }()
	}
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, server := range servers {
			if err := server.Shutdown(shutdownCtx); err != nil {
				return fmt.Errorf("shut down Codex proxy: %w", err)
			}
		}
		return ctx.Err()
	case err := <-errc:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, server := range servers {
			if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
				p.logger.Error("shut down Codex proxy listener", "error", shutdownErr)
			}
		}
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (p *Proxy) handle(w http.ResponseWriter, request *http.Request) {
	if err := p.authorizer.authorize(request); err != nil {
		p.logger.Warn("reject Codex remote connection", "error", err)
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	client, err := websocket.Accept(w, request, &websocket.AcceptOptions{
		// The Codex CLI is not a browser client and does not send a stable Origin.
		// The signed, environment-scoped bearer token is verified before upgrade.
		InsecureSkipVerify: true,
	})
	if err != nil {
		return
	}
	defer func() { _ = client.CloseNow() }()
	client.SetReadLimit(websocketReadLimit)

	upstream, _, err := websocket.Dial(request.Context(), p.settings.Upstream, nil)
	if err != nil {
		p.logger.Error("connect Codex app-server", "error", err)
		_ = client.Close(websocket.StatusInternalError, "Codex app-server unavailable")
		return
	}
	defer func() { _ = upstream.CloseNow() }()
	upstream.SetReadLimit(websocketReadLimit)

	var token Token
	if p.settings.effectiveAuthMode() == AuthModeSubscription {
		token, err = p.initialize(request.Context(), client, upstream)
		if err != nil {
			p.logger.Error("initialize Codex subscription proxy", "error", err)
			_ = client.Close(websocket.StatusInternalError, "Codex subscription authentication unavailable")
			return
		}
	}

	connection := &proxyConnection{
		client:   client,
		upstream: upstream,
		broker:   p.broker,
		logger:   p.logger,
		token:    token,
	}
	if err := connection.run(request.Context()); err != nil && !isNormalClose(err) {
		p.logger.Warn("Codex proxy connection closed", "error", err)
	}
}

func (p *Proxy) initialize(
	parent context.Context,
	client *websocket.Conn,
	upstream *websocket.Conn,
) (Token, error) {
	ctx, cancel := context.WithTimeout(parent, initializeTimeout)
	defer cancel()

	messageType, initialize, err := client.Read(ctx)
	if err != nil {
		return Token{}, fmt.Errorf("read initialize request: %w", err)
	}
	initialize, initializeID, err := enableExperimentalAPI(initialize)
	if err != nil {
		return Token{}, err
	}
	if err := upstream.Write(ctx, messageType, initialize); err != nil {
		return Token{}, fmt.Errorf("forward initialize request: %w", err)
	}
	if err := forwardUntilResponse(ctx, upstream, client, initializeID); err != nil {
		return Token{}, fmt.Errorf("forward initialize response: %w", err)
	}

	messageType, initialized, err := client.Read(ctx)
	if err != nil {
		return Token{}, fmt.Errorf("read initialized notification: %w", err)
	}
	if rpcMethod(initialized) != "initialized" {
		return Token{}, fmt.Errorf("expected initialized notification, got %q", rpcMethod(initialized))
	}
	if err := upstream.Write(ctx, messageType, initialized); err != nil {
		return Token{}, fmt.Errorf("forward initialized notification: %w", err)
	}

	token, err := p.broker.Token(ctx, "")
	if err != nil {
		return Token{}, err
	}
	login, err := loginRequest(token)
	if err != nil {
		return Token{}, err
	}
	if err := upstream.Write(ctx, websocket.MessageText, login); err != nil {
		return Token{}, fmt.Errorf("send external ChatGPT login: %w", err)
	}
	if err := discardUntilResponse(
		ctx,
		upstream,
		client,
		json.RawMessage(`"`+loginRequestID+`"`),
	); err != nil {
		return Token{}, fmt.Errorf("complete external ChatGPT login: %w", err)
	}

	return token, nil
}

type proxyConnection struct {
	client        *websocket.Conn
	upstream      *websocket.Conn
	broker        *resilientBroker
	logger        *slog.Logger
	upstreamWrite sync.Mutex
	clientWrite   sync.Mutex
	tokenMu       sync.Mutex
	token         Token
}

func (c *proxyConnection) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 2)
	go func() { errc <- c.clientToUpstream(ctx) }()
	go func() { errc <- c.upstreamToClient(ctx) }()
	err := <-errc
	cancel()
	return err
}

func (c *proxyConnection) clientToUpstream(ctx context.Context) error {
	for {
		messageType, payload, err := c.client.Read(ctx)
		if err != nil {
			return err
		}
		c.upstreamWrite.Lock()
		err = c.upstream.Write(ctx, messageType, payload)
		c.upstreamWrite.Unlock()
		if err != nil {
			return err
		}
	}
}

func (c *proxyConnection) upstreamToClient(ctx context.Context) error {
	for {
		messageType, payload, err := c.upstream.Read(ctx)
		if err != nil {
			return err
		}
		if c.broker != nil && rpcMethod(payload) == "account/chatgptAuthTokens/refresh" {
			if err := c.handleRefresh(ctx, payload); err != nil {
				return err
			}
			continue
		}
		c.clientWrite.Lock()
		err = c.client.Write(ctx, messageType, payload)
		c.clientWrite.Unlock()
		if err != nil {
			return err
		}
	}
}

func (c *proxyConnection) handleRefresh(ctx context.Context, request []byte) error {
	id, err := rpcID(request)
	if err != nil {
		return fmt.Errorf("decode Codex refresh request: %w", err)
	}
	c.tokenMu.Lock()
	previousHash := c.token.AccessHash
	c.tokenMu.Unlock()
	token, err := c.broker.Token(ctx, previousHash)
	if err != nil {
		return fmt.Errorf("refresh subscription token: %w", err)
	}
	response, err := refreshResponse(id, token)
	if err != nil {
		return err
	}
	c.tokenMu.Lock()
	c.token = token
	c.tokenMu.Unlock()
	c.upstreamWrite.Lock()
	err = c.upstream.Write(ctx, websocket.MessageText, response)
	c.upstreamWrite.Unlock()
	return err
}

func enableExperimentalAPI(payload []byte) ([]byte, json.RawMessage, error) {
	var message map[string]any
	if err := json.Unmarshal(payload, &message); err != nil {
		return nil, nil, fmt.Errorf("decode initialize request: %w", err)
	}
	if message["method"] != "initialize" {
		return nil, nil, fmt.Errorf("first Codex request must be initialize, got %q", message["method"])
	}
	params, ok := message["params"].(map[string]any)
	if !ok {
		params = map[string]any{}
		message["params"] = params
	}
	capabilities, ok := params["capabilities"].(map[string]any)
	if !ok {
		capabilities = map[string]any{}
		params["capabilities"] = capabilities
	}
	capabilities["experimentalApi"] = true
	id, err := json.Marshal(message["id"])
	if err != nil || string(id) == "null" {
		return nil, nil, errors.New("initialize request must contain an id")
	}
	normalized, err := json.Marshal(message)
	if err != nil {
		return nil, nil, fmt.Errorf("encode initialize request: %w", err)
	}
	return normalized, id, nil
}

func loginRequest(token Token) ([]byte, error) {
	return json.Marshal(map[string]any{
		"id":     loginRequestID,
		"method": "account/login/start",
		"params": map[string]any{
			"type":             "chatgptAuthTokens",
			"accessToken":      token.AccessToken,
			"chatgptAccountId": token.AccountID,
			"chatgptPlanType":  nullableString(token.PlanType),
		},
	})
}

func refreshResponse(id json.RawMessage, token Token) ([]byte, error) {
	var idValue any
	if err := json.Unmarshal(id, &idValue); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"id": idValue,
		"result": map[string]any{
			"accessToken":      token.AccessToken,
			"chatgptAccountId": token.AccountID,
			"chatgptPlanType":  nullableString(token.PlanType),
		},
	})
}

func forwardUntilResponse(
	ctx context.Context,
	from *websocket.Conn,
	to *websocket.Conn,
	id json.RawMessage,
) error {
	for {
		messageType, payload, err := from.Read(ctx)
		if err != nil {
			return err
		}
		if idsEqual(rpcIDOrNil(payload), id) {
			return to.Write(ctx, messageType, payload)
		}
		if err := to.Write(ctx, messageType, payload); err != nil {
			return err
		}
	}
}

func discardUntilResponse(
	ctx context.Context,
	from *websocket.Conn,
	to *websocket.Conn,
	id json.RawMessage,
) error {
	for {
		messageType, payload, err := from.Read(ctx)
		if err != nil {
			return err
		}
		if idsEqual(rpcIDOrNil(payload), id) {
			var response struct {
				Error json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(payload, &response); err != nil {
				return err
			}
			if len(response.Error) > 0 && string(response.Error) != "null" {
				return fmt.Errorf("Codex rejected external ChatGPT auth: %s", response.Error)
			}
			return nil
		}
		if err := to.Write(ctx, messageType, payload); err != nil {
			return err
		}
	}
}

func rpcMethod(payload []byte) string {
	var message struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(payload, &message)
	return message.Method
}

func rpcID(payload []byte) (json.RawMessage, error) {
	var message struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(payload, &message); err != nil {
		return nil, err
	}
	if len(message.ID) == 0 || string(message.ID) == "null" {
		return nil, errors.New("JSON-RPC request is missing id")
	}
	return message.ID, nil
}

func rpcIDOrNil(payload []byte) json.RawMessage {
	id, _ := rpcID(payload)
	return id
}

func idsEqual(left json.RawMessage, right json.RawMessage) bool {
	return len(left) > 0 && len(right) > 0 && string(left) == string(right)
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func isNormalClose(err error) bool {
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway
}
