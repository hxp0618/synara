package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/synara-ai/synara/services/control-plane/internal/developerapi"
)

type developerAPIResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *developerAPIResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *developerAPIResponseWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *developerAPIResponseWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *developerAPIResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func setDeveloperAPIRateLimitHeaders(header http.Header, admission developerapi.Admission) {
	header.Set("RateLimit-Limit", strconv.Itoa(admission.Limit))
	header.Set("RateLimit-Remaining", strconv.Itoa(admission.Remaining))
	header.Set("RateLimit-Reset", strconv.Itoa(developerAPIRetryAfterSeconds(admission.ResetAfter)))
}

func developerAPIRetryAfterSeconds(resetAfter time.Duration) int {
	return max(1, int((resetAfter+time.Second-1)/time.Second))
}
