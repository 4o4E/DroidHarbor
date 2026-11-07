package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"droidharbor/internal/events"
	"droidharbor/internal/identity"
	"droidharbor/internal/leases"
)

const streamAuthInterval = 5 * time.Second

func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request) {
	if s.Events == nil || s.Leases == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "FEATURE_UNAVAILABLE", "事件流不可用")
		return
	}
	scope, ok := s.eventScope(w, r)
	if !ok {
		return
	}
	sub, err := s.Events.Subscribe(scope)
	if err != nil {
		if errors.Is(err, events.ErrCapacity) {
			s.fail(w, r, http.StatusServiceUnavailable, "EVENT_CAPACITY", "事件连接容量已满")
		} else {
			s.fail(w, r, http.StatusServiceUnavailable, "FEATURE_UNAVAILABLE", "事件流不可用")
		}
		return
	}
	defer sub.Close()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	ticker := time.NewTicker(streamAuthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.Done:
			return
		case <-ticker.C:
			if _, valid := s.recheckEventScope(r, scope); !valid {
				return
			}
			if err := writeHeartbeat(w, controller); err != nil {
				return
			}
		case event, open := <-sub.C:
			if !open {
				return
			}
			current, valid := s.recheckEventScope(r, scope)
			if !valid {
				return
			}
			if !current.Admin && event.EntityType != "resync" && event.TaskID != current.TaskID {
				continue
			}
			select {
			case <-sub.Done:
				return
			default:
			}
			if err := writeSSE(w, controller, event); err != nil {
				return
			}
		}
	}
}

func (s *Server) eventScope(w http.ResponseWriter, r *http.Request) (events.Scope, bool) {
	if r.Header.Get("Authorization") != "" {
		secret, ok := s.bearer(w, r)
		if !ok {
			return events.Scope{}, false
		}
		principal, err := s.Leases.Authenticate(r.Context(), secret)
		if err != nil {
			s.domainError(w, r, err)
			return events.Scope{}, false
		}
		scope, ok := s.eventPrincipalScope(w, r, principal)
		return scope, ok
	}
	if _, ok := s.authorize(w, r, false); !ok {
		return events.Scope{}, false
	}
	return events.Scope{Admin: true}, true
}

func (s *Server) eventPrincipalScope(w http.ResponseWriter, r *http.Request, principal leases.Principal) (events.Scope, bool) {
	if principal.Kind != leases.PrincipalTask {
		s.fail(w, r, http.StatusForbidden, "FORBIDDEN", "事件流不允许此凭证访问")
		return events.Scope{}, false
	}
	return events.Scope{TaskID: principal.ID}, true
}

func (s *Server) recheckEventScope(r *http.Request, old events.Scope) (events.Scope, bool) {
	if err := r.Context().Err(); err != nil {
		return events.Scope{}, false
	}
	if old.Admin {
		cookie, err := r.Cookie(sessionCookie)
		if err != nil {
			return events.Scope{}, false
		}
		var id string
		err = s.DB.QueryRowContext(r.Context(), "SELECT id FROM admin_sessions WHERE secret_hash=? AND admin_credential_version=? AND revoked_at IS NULL AND expires_at>?", identity.Digest(cookie.Value), s.Admin.Version, time.Now().UTC().UnixMilli()).Scan(&id)
		return events.Scope{Admin: true}, err == nil
	}
	scheme, secret, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || secret == "" || strings.ContainsAny(secret, " \t\r\n") {
		return events.Scope{}, false
	}
	principal, err := s.Leases.Authenticate(r.Context(), secret)
	if err != nil || principal.Kind != leases.PrincipalTask || principal.ID != old.TaskID {
		return events.Scope{}, false
	}
	return events.Scope{TaskID: principal.ID}, true
}

func writeSSE(w http.ResponseWriter, controller *http.ResponseController, event events.Event) error {
	if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %s\nevent: change\ndata: %s\n\n", event.EventID, payload); err != nil {
		return err
	}
	if err := controller.Flush(); err != nil {
		return err
	}
	return controller.SetWriteDeadline(time.Time{})
}

func writeHeartbeat(w http.ResponseWriter, controller *http.ResponseController) error {
	if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
		return err
	}
	if err := controller.Flush(); err != nil {
		return err
	}
	return controller.SetWriteDeadline(time.Time{})
}
