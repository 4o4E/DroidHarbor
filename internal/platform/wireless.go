package platform

import (
	"net/http"

	"droidharbor/internal/adb"
)

func (s *Server) discoverWirelessDevices(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r, false); !ok {
		return
	}
	services, err := s.Devices.ScanWireless(r.Context())
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.Logger.Info("wireless_adb_scan_completed", "requestId", r.Context().Value(requestKey{}), "services", len(services))
	s.json(w, http.StatusOK, map[string]any{"services": services})
}

func (s *Server) connectWirelessDevice(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authorize(w, r, true)
	if !ok {
		return
	}
	var service adb.MDNSService
	if !s.decode(w, r, &service, false) {
		return
	}
	found, err := s.Devices.ConnectWireless(r.Context(), actor, service)
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.Logger.Info("wireless_adb_connected", "requestId", r.Context().Value(requestKey{}), "actorId", actor)
	s.json(w, http.StatusOK, map[string]any{"devices": found})
}

func (s *Server) pairWirelessDevice(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authorize(w, r, true)
	if !ok {
		return
	}
	var body struct {
		adb.MDNSService
		PairingCode string `json:"pairingCode"`
	}
	if !s.decode(w, r, &body, false) {
		return
	}
	if err := s.Devices.PairWireless(r.Context(), actor, body.MDNSService, body.PairingCode); err != nil {
		s.domainError(w, r, err)
		return
	}
	s.Logger.Info("wireless_adb_paired", "requestId", r.Context().Value(requestKey{}), "actorId", actor)
	w.WriteHeader(http.StatusNoContent)
}
