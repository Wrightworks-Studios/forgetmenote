package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"time"
)

// gatewayModulesKind is the per-gateway record of the modules each gateway of
// an instance turns off. Every gateway reads modules.off from its own
// instance.json, so two can disagree; the record lets each see the others.
const gatewayModulesKind = "gateway-modules"

// gatewayModules is a live gateway and the modules it turns off.
type gatewayModules struct {
	Gateway string   `json:"gateway"`
	This    bool     `json:"this"`
	Off     []string `json:"off"`
	// Same reports whether the gateway turns off the modules this one does.
	Same bool `json:"same"`
}

// recordGatewayModules writes the modules this gateway turns off where the
// instance's other gateways read them, and logs each live gateway that turns
// off others. A rolling restart that changes modules.off has a short mixed
// period, which this logs too.
func (s *Server) recordGatewayModules() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body, err := json.Marshal(map[string][]string{"off": s.modulesOff()})
	if err != nil {
		return err
	}
	if err = s.db.RecordGateway(ctx, gatewayModulesKind, body); err != nil {
		return err
	}
	gateways, err := s.gatewayModules(ctx)
	if err != nil {
		return err
	}
	for _, gateway := range gateways {
		if !gateway.Same {
			slog.Warn("another gateway of this instance turns off other modules; give every gateway the same modules.off", "gateway", gateway.Gateway, "off", strings.Join(gateway.Off, ", "), "this gateway", s.modules.String())
		}
	}
	return nil
}

// gatewayModules lists the instance's live gateways, this one first, with the
// modules each turns off.
func (s *Server) gatewayModules(ctx context.Context) ([]gatewayModules, error) {
	records, err := s.db.GatewayRecords(ctx, gatewayModulesKind)
	if err != nil {
		return nil, err
	}
	own := s.modulesOff()
	slices.Sort(own)
	out := []gatewayModules{}
	for holder, body := range records {
		var record struct {
			Off []string `json:"off"`
		}
		if err := json.Unmarshal(body, &record); err != nil {
			return nil, err
		}
		if record.Off == nil {
			record.Off = []string{}
		}
		off := slices.Clone(record.Off)
		slices.Sort(off)
		out = append(out, gatewayModules{Gateway: holder, This: holder == s.db.Holder, Off: record.Off, Same: slices.Equal(off, own)})
	}
	slices.SortFunc(out, func(a, b gatewayModules) int {
		if a.This != b.This {
			if a.This {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Gateway, b.Gateway)
	})
	return out, nil
}
