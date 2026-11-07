package platform

import (
	"net/http"

	"droidharbor/internal/devices"
	"droidharbor/internal/leases"
)

func (s *Server) discoverDevices(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r, false); !ok {
		return
	}
	found, err := s.Devices.Discover()
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.json(w, 200, map[string]any{"devices": found})
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	principal, ok := s.principal(w, r, false)
	if !ok {
		return
	}
	if principal.Kind == leases.PrincipalTask {
		s.domainError(w, r, leases.ErrForbidden)
		return
	}
	views, ready, err := s.Devices.List(r.Context(), principal.Kind == leases.PrincipalAdmin)
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	if id := r.PathValue("id"); id != "" {
		for _, view := range views {
			if view.ID == id {
				s.json(w, 200, map[string]any{"device": view, "discoveryReady": ready})
				return
			}
		}
		s.fail(w, r, 404, "DEVICE_NOT_FOUND", "设备不存在")
		return
	}
	s.json(w, 200, map[string]any{"devices": views, "discoveryReady": ready})
}

func (s *Server) registerDevice(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authorize(w, r, true)
	if !ok {
		return
	}
	var body devices.Registration
	if !s.decode(w, r, &body, false) {
		return
	}
	registered, err := s.Devices.Register(r.Context(), actor, body)
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.json(w, 201, registered)
}

func (s *Server) addDeviceLink(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authorize(w, r, true)
	if !ok {
		return
	}
	var body devices.LinkRequest
	if !s.decode(w, r, &body, false) {
		return
	}
	registered, err := s.Devices.AddLink(r.Context(), actor, r.PathValue("id"), body)
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.json(w, 201, registered)
}

func (s *Server) patchDeviceLink(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authorize(w, r, true)
	if !ok {
		return
	}
	var body devices.LinkPatch
	if !s.decode(w, r, &body, false) {
		return
	}
	if err := s.Devices.PatchLink(r.Context(), actor, r.PathValue("id"), r.PathValue("linkId"), body); err != nil {
		s.domainError(w, r, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) pairDeviceLink(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authorize(w, r, true)
	if !ok {
		return
	}
	var body struct {
		Endpoint string `json:"pairingEndpoint"`
		Code     string `json:"pairingCode"`
	}
	if !s.decode(w, r, &body, false) {
		return
	}
	if err := s.Devices.Pair(r.Context(), actor, r.PathValue("id"), r.PathValue("linkId"), body.Endpoint, body.Code); err != nil {
		s.domainError(w, r, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) connectDeviceLink(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authorize(w, r, true)
	if !ok {
		return
	}
	if !s.decode(w, r, &struct{}{}, true) {
		return
	}
	if err := s.Devices.Connect(r.Context(), actor, r.PathValue("id"), r.PathValue("linkId")); err != nil {
		s.domainError(w, r, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) verifyDeviceLink(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authorize(w, r, true)
	if !ok {
		return
	}
	if !s.decode(w, r, &struct{}{}, true) {
		return
	}
	evidence, err := s.Devices.Verify(r.Context(), actor, r.PathValue("id"), r.PathValue("linkId"))
	if err != nil {
		s.domainError(w, r, err)
		return
	}
	s.json(w, 200, evidence)
}

func (s *Server) patchDevice(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.authorize(w, r, true)
	if !ok {
		return
	}
	var body devices.Patch
	if !s.decode(w, r, &body, false) {
		return
	}
	if err := s.Devices.Patch(r.Context(), actor, r.PathValue("id"), body); err != nil {
		s.domainError(w, r, err)
		return
	}
	w.WriteHeader(204)
}

func (s *Server) recoverDevice(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r, true); !ok {
		return
	}
	if !s.decode(w, r, &struct{}{}, true) {
		return
	}
	if err := s.Leases.RetryRecovery(r.Context(), r.PathValue("id")); err != nil {
		s.domainError(w, r, err)
		return
	}
	w.WriteHeader(204)
}
