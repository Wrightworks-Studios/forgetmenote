package client

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"time"
)

func randomValue() string {
	var b [32]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func login(ctx context.Context, server string, noBrowser bool, diagnostics io.Writer, dataAccess string, manage bool) (Session, error) {
	if dataAccess != "" && dataAccess != "read" && dataAccess != "write" {
		return Session{}, errors.New("data-access must be read or write")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	server, err := ValidateServer(server)
	if err != nil {
		return Session{}, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Session{}, err
	}
	defer listener.Close()
	redirect := "http://" + listener.Addr().String() + "/callback"
	state, verifier := randomValue(), randomValue()
	challenge := sha256.Sum256([]byte(verifier))
	registration, _ := json.Marshal(map[string]any{"client_name": "ForgetMeNote CLI", "redirect_uris": []string{redirect}, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"}})
	r, err := http.NewRequestWithContext(ctx, "POST", server+"/oauth/register", bytes.NewReader(registration))
	if err != nil {
		return Session{}, err
	}
	r.Header.Set("Content-Type", "application/json")
	h := http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := h.Do(r)
	if err != nil {
		return Session{}, err
	}
	var client struct {
		ID string `json:"client_id"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&client)
	response.Body.Close()
	if err != nil || response.StatusCode != 201 || client.ID == "" {
		return Session{}, errors.New("OAuth client registration failed")
	}
	code := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state || r.URL.Query().Get("code") == "" {
			http.Error(w, "Invalid authorization callback", 400)
			return
		}
		select {
		case code <- r.URL.Query().Get("code"):
			fmt.Fprint(w, "Signed in. Return to your terminal.")
		default:
			http.Error(w, "Authorization already received", 409)
		}
	})
	callback := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer callback.Close()
	go callback.Serve(listener)
	values := url.Values{"client_id": {client.ID}, "redirect_uri": {redirect}, "response_type": {"code"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}, "scope": {"notes:read notes:write"}, "resource": {server + "/api/v1"}, "state": {state}}
	// Management consent can mint credentials and change access; request it
	// only for an explicit --manage sign-in.
	if manage {
		values.Set("scope", values.Get("scope")+" notes:manage")
	}
	if dataAccess != "" {
		values.Set("scope", values.Get("scope")+" data:read")
	}
	if dataAccess == "write" {
		values.Set("scope", values.Get("scope")+" data:write")
	}
	authorization := server + "/oauth/authorize?" + values.Encode()
	fmt.Fprintln(diagnostics, "Open this URL to sign in:\n"+authorization)
	if !noBrowser {
		program := "xdg-open"
		if runtime.GOOS == "darwin" {
			program = "open"
		}
		cmd := exec.CommandContext(ctx, program, authorization)
		if err = cmd.Start(); err == nil {
			go cmd.Wait()
		}
	}
	select {
	case value := <-code:
		var result tokenResponse
		if err = postForm(ctx, server+"/oauth/token", url.Values{"grant_type": {"authorization_code"}, "client_id": {client.ID}, "redirect_uri": {redirect}, "code": {value}, "code_verifier": {verifier}, "resource": {server + "/api/v1"}}, &result); err != nil {
			return Session{}, err
		}
		if result.AccessToken == "" || result.RefreshToken == "" {
			return Session{}, errors.New("invalid login response")
		}
		return Session{Server: server, Token: result.AccessToken, Refresh: result.RefreshToken, ClientID: client.ID, Expires: time.Now().Unix() + result.ExpiresIn}, nil
	case <-ctx.Done():
		return Session{}, ctx.Err()
	}
}
