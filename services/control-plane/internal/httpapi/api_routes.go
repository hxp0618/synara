package httpapi

import (
	"net/http"
	"strings"
)

// apiExposure is the externally visible stability tier for a registered route.
// A route cannot be registered without choosing a tier, so new endpoints fail
// closed instead of becoming part of the developer API by accident.
type apiExposure string

const (
	apiExposureInternal   apiExposure = "internal"
	apiExposurePublicBeta apiExposure = "public-beta"
	apiExposurePublicGA   apiExposure = "public-ga"
)

type apiRoute struct {
	Method   string
	Path     string
	Exposure apiExposure
}

type classifiedServeMux struct {
	mux    *http.ServeMux
	routes []apiRoute
}

func newClassifiedServeMux() *classifiedServeMux {
	return &classifiedServeMux{mux: http.NewServeMux()}
}

func (m *classifiedServeMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mux.ServeHTTP(w, r)
}

func (m *classifiedServeMux) Internal(pattern string, handler http.Handler) {
	m.register(pattern, handler, apiExposureInternal)
}

func (m *classifiedServeMux) InternalFunc(pattern string, handler http.HandlerFunc) {
	m.register(pattern, handler, apiExposureInternal)
}

func (m *classifiedServeMux) PublicBeta(pattern string, handler http.Handler) {
	m.register(pattern, handler, apiExposurePublicBeta)
}

func (m *classifiedServeMux) PublicBetaFunc(pattern string, handler http.HandlerFunc) {
	m.register(pattern, handler, apiExposurePublicBeta)
}

func (m *classifiedServeMux) PublicGA(pattern string, handler http.Handler) {
	m.register(pattern, handler, apiExposurePublicGA)
}

func (m *classifiedServeMux) PublicGAFunc(pattern string, handler http.HandlerFunc) {
	m.register(pattern, handler, apiExposurePublicGA)
}

func (m *classifiedServeMux) register(pattern string, handler http.Handler, exposure apiExposure) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok || strings.TrimSpace(method) == "" || !strings.HasPrefix(path, "/") {
		panic("httpapi: classified route must use a method and absolute path")
	}
	m.routes = append(m.routes, apiRoute{Method: method, Path: path, Exposure: exposure})
	m.mux.Handle(pattern, handler)
}

func (m *classifiedServeMux) manifest() []apiRoute {
	return append([]apiRoute(nil), m.routes...)
}
