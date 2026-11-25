package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The probe contacts the existing process; it never opens state or credentials.
func runHealth(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("forgetmenote healthz", flag.ContinueOnError)
	address := flags.String("url", "http://127.0.0.1:8080/healthz", "running application health endpoint")
	if err := flags.Parse(args); err != nil {
		return err
	}
	parsed, err := url.Parse(*address)
	if err != nil || flags.NArg() != 0 || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("healthz requires an HTTP(S) URL without credentials")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("health endpoint unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("health check failed (HTTP %d)", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1025))
	if err != nil || len(raw) > 1024 || strings.TrimSpace(string(raw)) != "ok" {
		return errors.New("invalid health response")
	}
	return nil
}
