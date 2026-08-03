package httpapi

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/desktopenrollment"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func (s *Server) getPlatformDesktopAccess(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	access, err := s.desktopEnrollment.ListPlatform(r.Context(), mustPrincipal(r), tenantID, requestID(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, access)
}

func (s *Server) issuePlatformDesktopEnrollment(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := s.pathUUID(w, r, "tenantID")
	if !ok {
		return
	}
	var input desktopenrollment.IssueInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	issued, err := s.desktopEnrollment.Issue(
		r.Context(), mustPrincipal(r), tenantID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, issued)
}

func (s *Server) markPlatformDesktopEnrollmentOpened(w http.ResponseWriter, r *http.Request) {
	enrollmentID, ok := s.pathUUID(w, r, "enrollmentID")
	if !ok {
		return
	}
	var input desktopenrollment.VersionInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	enrollment, err := s.desktopEnrollment.MarkOpened(
		r.Context(), mustPrincipal(r), enrollmentID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, enrollment)
}

func (s *Server) revokePlatformDesktopEnrollment(w http.ResponseWriter, r *http.Request) {
	enrollmentID, ok := s.pathUUID(w, r, "enrollmentID")
	if !ok {
		return
	}
	var input desktopenrollment.RevokeInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	enrollment, err := s.desktopEnrollment.RevokeEnrollment(
		r.Context(), mustPrincipal(r), enrollmentID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, enrollment)
}

func (s *Server) revokePlatformDesktopDevice(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := s.pathUUID(w, r, "deviceID")
	if !ok {
		return
	}
	var input desktopenrollment.RevokeInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	device, err := s.desktopEnrollment.RevokeDevice(
		r.Context(), mustPrincipal(r), deviceID, input, requestID(r), clientIP(r),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, device)
}

func (s *Server) redeemDesktopEnrollment(w http.ResponseWriter, r *http.Request) {
	address := strings.TrimSpace(clientIP(r))
	if !s.desktopRedeemLimit.Allow(address, time.Now().UTC()) {
		s.writeError(w, r, problem.New(429, "desktop_enrollment_rate_limited", "Desktop Enrollment could not be redeemed."))
		return
	}
	var input desktopenrollment.RedeemInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	issued, err := s.desktopEnrollment.Redeem(
		r.Context(), input, requestID(r), address, r.UserAgent(),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, issued)
}

func (s *Server) rotateDesktopSession(w http.ResponseWriter, r *http.Request) {
	var input desktopenrollment.RotateInput
	if err := decodeJSON(r, &input); err != nil {
		s.writeError(w, r, err)
		return
	}
	rotated, err := s.desktopEnrollment.Rotate(
		r.Context(), mustPrincipal(r), input, requestID(r), clientIP(r), r.UserAgent(),
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, rotated)
}

func (s *Server) disconnectDesktop(w http.ResponseWriter, r *http.Request) {
	if err := s.desktopEnrollment.Disconnect(
		r.Context(), mustPrincipal(r), requestID(r), clientIP(r),
	); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type desktopRedemptionWindow struct {
	startedAt time.Time
	count     int
}

type desktopRedemptionRateLimiter struct {
	mu       sync.Mutex
	limit    int
	interval time.Duration
	windows  map[string]desktopRedemptionWindow
}

func newDesktopRedemptionRateLimiter(limit int, interval time.Duration) *desktopRedemptionRateLimiter {
	return &desktopRedemptionRateLimiter{
		limit: limit, interval: interval, windows: make(map[string]desktopRedemptionWindow),
	}
}

func (l *desktopRedemptionRateLimiter) Allow(key string, now time.Time) bool {
	if l == nil || l.limit <= 0 || l.interval <= 0 {
		return false
	}
	if key == "" {
		key = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	window := l.windows[key]
	if window.startedAt.IsZero() || !now.Before(window.startedAt.Add(l.interval)) {
		window = desktopRedemptionWindow{startedAt: now}
	}
	if window.count >= l.limit {
		l.windows[key] = window
		return false
	}
	window.count++
	l.windows[key] = window
	if len(l.windows) > 4096 {
		for candidate, value := range l.windows {
			if !now.Before(value.startedAt.Add(l.interval)) {
				delete(l.windows, candidate)
			}
		}
	}
	return true
}
